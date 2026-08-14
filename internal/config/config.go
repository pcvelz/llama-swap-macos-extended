package config

import (
	"fmt"
	"os"
	"path"
	"sort"

	"gopkg.in/yaml.v3"
)

const DEFAULT_GROUP_ID = "(default)"
const DEFAULT_UNLOAD_TIMEOUT = 10
const (
	LogToStdoutProxy    = "proxy"
	LogToStdoutUpstream = "upstream"
	LogToStdoutBoth     = "both"
	LogToStdoutNone     = "none"
)

type MacroEntry struct {
	Name  string
	Value any
}

type MacroList []MacroEntry

// UnmarshalYAML implements custom YAML unmarshaling that preserves macro definition order
func (ml *MacroList) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.MappingNode {
		return fmt.Errorf("macros must be a mapping")
	}

	// yaml.Node.Content for a mapping contains alternating key/value nodes
	entries := make([]MacroEntry, 0, len(value.Content)/2)
	for i := 0; i < len(value.Content); i += 2 {
		keyNode := value.Content[i]
		valueNode := value.Content[i+1]

		var name string
		if err := keyNode.Decode(&name); err != nil {
			return fmt.Errorf("failed to decode macro name: %w", err)
		}

		var val any
		if err := valueNode.Decode(&val); err != nil {
			return fmt.Errorf("failed to decode macro value for '%s': %w", name, err)
		}

		entries = append(entries, MacroEntry{Name: name, Value: val})
	}

	*ml = entries
	return nil
}

// Get retrieves a macro value by name
func (ml MacroList) Get(name string) (any, bool) {
	for _, entry := range ml {
		if entry.Name == name {
			return entry.Value, true
		}
	}
	return nil, false
}

type GroupConfig struct {
	Swap       bool     `yaml:"swap"`
	Exclusive  bool     `yaml:"exclusive"`
	Persistent bool     `yaml:"persistent"`
	Members    []string `yaml:"members"`
}

// set default values for GroupConfig
func (c *GroupConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	type rawGroupConfig GroupConfig
	defaults := rawGroupConfig{
		Swap:       true,
		Exclusive:  true,
		Persistent: false,
		Members:    []string{},
	}

	if err := unmarshal(&defaults); err != nil {
		return err
	}

	*c = GroupConfig(defaults)
	return nil
}

type HooksConfig struct {
	OnStartup HookOnStartup `yaml:"on_startup"`
}

type HookOnStartup struct {
	Preload []string `yaml:"preload"`
}

type Store struct {
	Path string `yaml:"path"`
}

type UIConfig struct {
	Activity UIActivityConfig `yaml:"activity" json:"activity"`
}

type UIActivityConfig struct {
	SessionID []string `yaml:"session_id" json:"session_id"`
}

// ProfileConfig describes a runtime-selectable set of model ID rewrites.
// Empty pin targets disable the corresponding model ID while the profile is
// active. YAML null values decode to the same empty string representation.
type ProfileConfig struct {
	Description string            `yaml:"description" json:"description"`
	Pins        map[string]string `yaml:"pins" json:"pins"`
}

// UnmarshalYAML rejects the removed list-shaped profile syntax with a useful
// migration error while allowing null pin values to normalize to empty strings.
func (c *ProfileConfig) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.MappingNode {
		return fmt.Errorf("profile must be a mapping with description and pins; the legacy list syntax is no longer supported")
	}
	type rawProfileConfig ProfileConfig
	var raw rawProfileConfig
	if err := value.Decode(&raw); err != nil {
		return err
	}
	*c = ProfileConfig(raw)
	return nil
}

type Config struct {
	HealthCheckTimeout int               `yaml:"healthCheckTimeout"`
	LogRequests        bool              `yaml:"logRequests"`
	LogLevel           string            `yaml:"logLevel"`
	LogTimeFormat      string            `yaml:"logTimeFormat"`
	LogToStdout        string            `yaml:"logToStdout"`
	MetricsMaxInMemory int               `yaml:"metricsMaxInMemory"`
	CaptureBuffer      int               `yaml:"captureBuffer"`
	Store              *Store            `yaml:"store"`
	UI                 UIConfig          `yaml:"ui"`
	Performance        PerformanceConfig `yaml:"performance"`
	GlobalTTL          int               `yaml:"globalTTL"`
	UnloadTimeout      int               `yaml:"unloadTimeout"`

	// SwapGraceSeconds is the global default for per-model swapGraceSeconds: a
	// request for a non-resident model waits in the queue until the resident has
	// been idle this long before it is evicted. 0 (default) = evict as soon as
	// the resident drains (upstream behaviour).
	SwapGraceSeconds int `yaml:"swapGraceSeconds"`

	Models    map[string]ModelConfig    `yaml:"models"` /* key is model ID */
	Profiles  map[string]ProfileConfig  `yaml:"profiles"`
	Selectors map[string]SelectorConfig `yaml:"selectors"`

	// routing is the canonical source for swap/scheduling configuration.
	// New code must read Routing, never the backwards-compat fields below.
	Routing RoutingConfig `yaml:"routing"`

	// Groups and Matrix are permanent backwards-compat input fields for the
	// legacy top-level `groups:`/`matrix:` keys. They are normalized into
	// Routing by LoadConfigFromReader. New code must not read them directly.
	Groups map[string]GroupConfig `yaml:"groups"` /* key is group ID */
	Matrix *MatrixConfig          `yaml:"matrix"`

	// for key/value replacements in model's cmd, cmdStop, proxy, checkEndPoint
	Macros MacroList `yaml:"macros"`

	// map aliases to actual model IDs
	aliases map[string]string

	// automatic port assignments
	StartPort int `yaml:"startPort"`

	// hooks, see: #209
	Hooks HooksConfig `yaml:"hooks"`

	// send loading state in reasoning
	SendLoadingState bool `yaml:"sendLoadingState"`

	// present aliases to /v1/models OpenAI API listing
	IncludeAliasesInList bool `yaml:"includeAliasesInList"`

	// ResidentAliases lists model-id patterns (shell globs, e.g.
	// "claude-haiku-*", or literals like "default") that resolve at request
	// time to whichever local model is CURRENTLY resident (state ready),
	// instead of to a fixed model block. Unlike a static alias this never
	// triggers a load or swap: with nothing resident the request 404s exactly
	// as before. Patterns may not collide with a real model id or static
	// alias — a shadowed resident alias would be dead config.
	ResidentAliases []string `yaml:"residentAliases"`

	// menu-bar (macOS) / system-tray (Windows, Linux) helper; accepts a bool
	// (legacy) or a mapping with `enabled` and `bars`, see MenuBarConfig.
	MenuBar MenuBarConfig `yaml:"menu_bar"`

	// support API keys, see issue #433, #50, #251
	RequiredAPIKeys []string `yaml:"apiKeys"`

	// support remote peers, see issue #433, #296
	Peers PeerDictionaryConfig `yaml:"peers"`

	// upstream controls behaviour of the /upstream passthrough endpoint
	Upstream UpstreamConfig `yaml:"upstream"`

	// Tiers declares extra HTTP entry points into the one shared FIFO queue,
	// each pre-tagging every request that arrives through it with a rank
	// before it joins the queue. The main `-listen` port is always the
	// implicit default tier (rank 0) and is never declared here - an absent
	// or empty Tiers map is byte-identical, single-listener behavior. See
	// docs/intent/llama-swap-tiers.md (llama-cm) for the full design.
	Tiers map[string]TierConfig `yaml:"tiers"`
}

// TierConfig is one extra entry point declared under the top-level `tiers:`
// block. See Config.Tiers.
type TierConfig struct {
	// Listen is this tier's own listen address (required, e.g. "127.0.0.1:8002").
	Listen string `yaml:"listen"`
	// Rank orders the shared queue: higher ranks are serviced first, and a
	// queued request is never granted while a strictly higher-rank request is
	// still queued. The implicit default tier is rank 0; any nonzero rank is
	// allowed here, including negative (background) ranks.
	Rank int `yaml:"rank"`
	// Preempts, when true, lets an arrival on this tier boot ANY running
	// lower-rank request, including non-preemptible ones.
	Preempts bool `yaml:"preempts"`
	// Preemptible, when true, lets a running request on this tier be booted
	// by any higher-rank arrival, even one without Preempts set.
	Preemptible bool `yaml:"preemptible"`
}

// RoutingConfig is the canonical, normalized routing/scheduling configuration.
type RoutingConfig struct {
	Scheduler SchedulerConfig `yaml:"scheduler"`
	Router    RouterConfig    `yaml:"router"`
}

type SchedulerConfig struct {
	Use      string            `yaml:"use"` // default "fifo"
	Settings SchedulerSettings `yaml:"settings"`
}

type SchedulerSettings struct {
	Fifo FifoConfig `yaml:"fifo"`
}

type FifoConfig struct {
	Priority map[string]int `yaml:"priority"` // model ID -> priority, default 0
	// KVPoolTokens is a per-model KV-aware admission budget, in ESTIMATED
	// tokens (see shared.EstimateTokens — buffered request body bytes / 4).
	// It exists for models served with a single shared/unified KV pool across
	// multiple parallel slots (e.g. llama.cpp --parallel 2 --kv-unified): two
	// concurrent large-context requests can together exceed the pool and abort
	// BOTH. When a model's budget here is > 0, the FIFO scheduler never
	// forwards a request that would push the model's in-flight estimated
	// tokens over this ceiling — it parks the request in the normal queue
	// until enough of the pool frees up. 0 or absent (the default) disables
	// admission control for that model entirely (today's behavior, fail-open).
	KVPoolTokens map[string]int `yaml:"kvPoolTokens"`
}

type RouterConfig struct {
	Use      string         `yaml:"use"` // "group" (default) | "matrix"
	Settings RouterSettings `yaml:"settings"`
}

type RouterSettings struct {
	Groups map[string]GroupConfig `yaml:"groups"`
	Matrix *MatrixConfig          `yaml:"matrix"`
}

func (c *Config) RealModelName(search string) (string, bool) {
	if _, found := c.Models[search]; found {
		return search, true
	} else if name, found := c.aliases[search]; found {
		return name, found
	} else {
		return "", false
	}
}

// MatchesResidentAlias reports whether the requested model id matches one of
// the configured resident-alias patterns. Pattern errors are impossible here:
// patterns are validated at config load.
func (c *Config) MatchesResidentAlias(requested string) bool {
	for _, pattern := range c.ResidentAliases {
		if ok, _ := path.Match(pattern, requested); ok {
			return true
		}
	}
	return false
}

func (c *Config) FindConfig(modelName string) (ModelConfig, string, bool) {
	if realName, found := c.RealModelName(modelName); !found {
		return ModelConfig{}, "", false
	} else {
		return c.Models[realName], realName, true
	}
}

// ResolveBaseModel resolves a name without applying profiles. Local model IDs
// and aliases take precedence over peer model IDs, matching server dispatch.
func (c *Config) ResolveBaseModel(search string) (string, bool) {
	if realName, found := c.RealModelName(search); found {
		return realName, true
	}
	if _, _, found := c.ResolvePeerModel(search); found {
		return search, true
	}
	return "", false
}

func LoadConfig(path string) (Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return Config{}, err
	}
	defer file.Close()
	return LoadConfigFromReader(file)
}

// rewrites the yaml to include a default group with any orphaned models
func AddDefaultGroupToConfig(config Config) Config {

	if config.Groups == nil {
		config.Groups = make(map[string]GroupConfig)
	}

	defaultGroup := GroupConfig{
		Swap:      true,
		Exclusive: true,
		Members:   []string{},
	}
	// if groups is empty, create a default group and put
	// all models into it
	if len(config.Groups) == 0 {
		for modelName := range config.Models {
			defaultGroup.Members = append(defaultGroup.Members, modelName)
		}
	} else {
		// iterate over existing group members and add non-grouped models into the default group
		for modelName := range config.Models {
			foundModel := false
		found:
			// search for the model in existing groups
			for _, groupConfig := range config.Groups {
				for _, member := range groupConfig.Members {
					if member == modelName {
						foundModel = true
						break found
					}
				}
			}

			if !foundModel {
				defaultGroup.Members = append(defaultGroup.Members, modelName)
			}
		}
	}

	sort.Strings(defaultGroup.Members) // make consistent ordering for testing
	config.Groups[DEFAULT_GROUP_ID] = defaultGroup

	return config
}
