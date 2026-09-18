package config

import (
	"errors"
	"fmt"
	"runtime"
)

const (
	MODEL_CONFIG_DEFAULT_TTL        = -1
	MODEL_CONFIG_DEFAULT_SWAP_GRACE = -1
	MODEL_CONFIG_DEFAULT_PROXY      = "http://localhost:${PORT}"
	comfyUIConcurrencyLimit         = 50

	// ComfyUIModelID identifies the model used by the /comfyui endpoint.
	ComfyUIModelID = "comfyui_auto"

	// MODEL_CONFIG_DEFAULT_MAX_PARALLEL_LARGE_PREFILL is maxParallelLargePrefill
	// when a model leaves it unset: large requests serialize.
	MODEL_CONFIG_DEFAULT_MAX_PARALLEL_LARGE_PREFILL = 1
)

var validModalities = map[string]struct{}{
	"text":  {},
	"audio": {},
	"image": {},
	"video": {},
}

// ModelCapConfig defines what modalities and features a model supports.
// Used in /v1/models to inform clients. An empty block (all zero values) is
// treated as not configured.
type ModelCapConfig struct {
	In       []string `yaml:"in"`
	Out      []string `yaml:"out"`
	Tools    bool     `yaml:"tools"`
	Reranker bool     `yaml:"reranker"`
	Context  int      `yaml:"context"`
}

// Empty returns true when all fields are at their zero values.
func (c ModelCapConfig) Empty() bool {
	return len(c.In) == 0 && len(c.Out) == 0 && !c.Tools && !c.Reranker && c.Context == 0
}

// Validate checks that all modality values are recognized and context is
// non-negative. Returns an error if any value is invalid.
func (c ModelCapConfig) Validate() error {
	for _, m := range c.In {
		if _, ok := validModalities[m]; !ok {
			return fmt.Errorf("capabilities.in: invalid modality %q, must be one of: text, audio, image, video", m)
		}
	}
	for _, m := range c.Out {
		if _, ok := validModalities[m]; !ok {
			return fmt.Errorf("capabilities.out: invalid modality %q, must be one of: text, audio, image, video", m)
		}
	}
	if c.Context < 0 {
		return errors.New("capabilities.context: must be >= 0")
	}
	return nil
}

// TimeoutsConfig holds timeout settings for proxy connections
// 0 = no timeout
type TimeoutsConfig struct {
	Connect        int `yaml:"connect"`
	KeepAlive      int `yaml:"keepalive"`
	ResponseHeader int `yaml:"responseHeader"`
	TLSHandshake   int `yaml:"tlsHandshake"`
	ExpectContinue int `yaml:"expectContinue"`
	IdleConn       int `yaml:"idleConn"`
}

// CompatConfig holds compatibility settings for upstream applications.
type CompatConfig struct {
	// IgnoreWebsockets prevents websocket connections from participating in
	// model lifecycle activity such as swapping, concurrency, and TTL tracking.
	IgnoreWebsockets bool `yaml:"ignoreWebsockets"`
}

type ModelConfig struct {
	Cmd           string   `yaml:"cmd"`
	CmdStop       string   `yaml:"cmdStop"`
	Proxy         string   `yaml:"proxy"`
	Aliases       []string `yaml:"aliases"`
	Env           []string `yaml:"env"`
	CheckEndpoint string   `yaml:"checkEndpoint"`
	UnloadAfter   int      `yaml:"ttl"`

	// SwapGraceSeconds holds a request for a DIFFERENT model in the queue until
	// this model has had no in-flight requests for this many seconds before it
	// may be evicted to free the slot. It stops a momentary request gap (e.g. an
	// agent pausing to run a tool, then resuming) from evicting a still-hot model
	// the instant it drains. -1 (default) inherits the global swapGraceSeconds;
	// 0 disables the grace (evict as soon as the model drains — upstream behaviour).
	SwapGraceSeconds int `yaml:"swapGraceSeconds"`

	UnloadTimeout int    `yaml:"unloadTimeout"`
	Unlisted      bool   `yaml:"unlisted"`
	UseModelName  string `yaml:"useModelName"`

	// #179 for /v1/models
	Name        string `yaml:"name"`
	Description string `yaml:"description"`

	// Limit concurrency of HTTP requests to process
	ConcurrencyLimit int `yaml:"concurrencyLimit"`

	// Model filters see issue #174
	Filters ModelFilters `yaml:"filters"`

	// Macros: see #264
	// Model level macros take precedence over the global macros
	Macros MacroList `yaml:"macros"`

	// Metadata: see #264
	// Arbitrary metadata that can be exposed through the API
	Metadata map[string]any `yaml:"metadata"`

	// override global setting
	SendLoadingState *bool `yaml:"sendLoadingState"`

	// Timeout settings for proxy connections
	Timeouts TimeoutsConfig `yaml:"timeouts"`

	// Compatibility settings for upstream applications.
	Compat CompatConfig `yaml:"compat"`

	// Capabilities defines what modalities and features the model supports.
	Capabilities ModelCapConfig `yaml:"capabilities"`

	// PrefillTokensPerSecond is this model's conservative prompt-prefill
	// throughput, used by deadline-aware admission (router.baseRouter's
	// deadlineRefuse) to decide whether a preemptible request's prefill can
	// still finish inside the client's remaining zero-byte budget. 0 or absent
	// (the default) means "use the router's built-in conservative default".
	// Deliberately a per-model number: prefill rate is a property of the model
	// and its server flags, not of the request.
	PrefillTokensPerSecond float64 `yaml:"prefillTokensPerSecond"`

	// SlotAffinity pins a client session to the llama-server slot that served
	// its previous request. When true, llama-swap remembers the id_slot the
	// child reported on each response (keyed by the request's session_id, see
	// swaputil.extractContext) and injects it as `id_slot` into the next JSON
	// body from the same session, so llama.cpp reuses that slot's KV prefix
	// instead of picking a slot by its own LCP heuristic. Off by default; a
	// model whose child runs a single slot gains nothing from it. See
	// internal/server/slot_affinity.go.
	SlotAffinity bool `yaml:"slotAffinity"`

	// MaxParallelLargePrefill caps how many LARGE requests (estimated at or
	// above the scheduler's large-prefill threshold, 8192 tokens) of this model
	// may be granted at the same time. It sits inside ConcurrencyLimit, which
	// stays the outer cap on all requests, and next to the kvPoolTokens sum
	// check, which still applies. WHY a separate count: on a --kv-unified
	// multi-slot child the pool sum can admit two large requests whose
	// concurrent prefill compute buffers still exhaust GPU memory on one model
	// (a 27B dense model OOMs Metal), while another model's slots run large
	// turns in parallel fine. Parallelism is a property of the model, so it is
	// configured per model. Unset = MODEL_CONFIG_DEFAULT_MAX_PARALLEL_LARGE_PREFILL
	// (1: large requests serialize, the safe default for a new model); 0 or a
	// value >= ConcurrencyLimit = no separate cap. Only consulted for models
	// with a kvPoolTokens budget.
	MaxParallelLargePrefill int `yaml:"maxParallelLargePrefill"`

	// Copy of HealthCheckTimeout from global config
	HealthCheckTimeout int `yaml:"healthCheckTimeout"`
}

func (m *ModelConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	type rawModelConfig ModelConfig
	defaults := rawModelConfig{
		Cmd:              "",
		CmdStop:          "",
		Proxy:            MODEL_CONFIG_DEFAULT_PROXY,
		Aliases:          []string{},
		Env:              []string{},
		CheckEndpoint:    "/health",
		UnloadAfter:      MODEL_CONFIG_DEFAULT_TTL,        // use GlobalTTL
		SwapGraceSeconds: MODEL_CONFIG_DEFAULT_SWAP_GRACE, // use global swapGraceSeconds
		UnloadTimeout:    0,                               // use global UnloadTimeout
		Unlisted:         false,
		UseModelName:     "",
		ConcurrencyLimit: 0,
		Name:             "",
		Description:      "",

		// matches http.DefaultTransport
		Timeouts: TimeoutsConfig{
			Connect:        30,
			KeepAlive:      30,
			ResponseHeader: 0,
			TLSHandshake:   10,
			ExpectContinue: 1,
			IdleConn:       90,
		},
	}

	defaults.MaxParallelLargePrefill = MODEL_CONFIG_DEFAULT_MAX_PARALLEL_LARGE_PREFILL

	// the default cmdStop to taskkill /f /t /pid ${PID}
	if runtime.GOOS == "windows" {
		defaults.CmdStop = "taskkill /f /t /pid ${PID}"
	}

	if err := unmarshal(&defaults); err != nil {
		return err
	}

	*m = ModelConfig(defaults)
	return nil
}

func (m *ModelConfig) SanitizedCommand() ([]string, error) {
	return SanitizeCommand(m.Cmd)
}

// ModelFilters embeds Filters and adds legacy support for strip_params field
// See issue #174
type ModelFilters struct {
	Filters `yaml:",inline"`
}

func (m *ModelFilters) UnmarshalYAML(unmarshal func(interface{}) error) error {
	type rawModelFilters ModelFilters
	defaults := rawModelFilters{}

	if err := unmarshal(&defaults); err != nil {
		return err
	}

	// Try to unmarshal with the old field name for backwards compatibility
	if defaults.StripParams == "" {
		var legacy struct {
			StripParams string `yaml:"strip_params"`
		}
		if legacyErr := unmarshal(&legacy); legacyErr != nil {
			return errors.New("failed to unmarshal legacy filters.strip_params: " + legacyErr.Error())
		}
		defaults.StripParams = legacy.StripParams
	}

	*m = ModelFilters(defaults)
	return nil
}

// SanitizedStripParams wraps Filters.SanitizedStripParams for backwards compatibility
// Returns ([]string, error) to match existing API
func (f ModelFilters) SanitizedStripParams() ([]string, error) {
	return f.Filters.SanitizedStripParams(), nil
}
