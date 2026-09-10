package swaputil

import "time"

const ProcessStateChangeEventID = 0x01
const ConfigFileChangedEventID = 0x03
const ActivityLogEventID = 0x05
const ModelPreloadedEventID = 0x06
const InFlightRequestsEventID = 0x07
const ProfileChangedEventID = 0x08

// ProcessStateChangeEvent is emitted whenever a process transitions between
// lifecycle states. States are carried as strings so this package stays a leaf
// (no import of internal/process).
type ProcessStateChangeEvent struct {
	ProcessName string
	OldState    string
	NewState    string
}

func (e ProcessStateChangeEvent) Type() uint32 {
	return ProcessStateChangeEventID
}

type ReloadingState int

const (
	ReloadingStateStart ReloadingState = iota
	ReloadingStateEnd
)

type ConfigFileChangedEvent struct {
	State ReloadingState
}

func (e ConfigFileChangedEvent) Type() uint32 {
	return ConfigFileChangedEventID
}

type ModelPreloadedEvent struct {
	ModelName string
	Success   bool
}

func (e ModelPreloadedEvent) Type() uint32 {
	return ModelPreloadedEventID
}

type InFlightRequestsEvent struct {
	Total int `json:"total"`
	// ByTier is the in-flight/waiting count per tier name (including
	// "default"). Populated only when more than one tier is configured
	// (see docs/intent/llama-swap-tiers.md, "Inflight/menu" section); nil
	// otherwise so single-listener deployments see byte-identical payloads.
	ByTier map[string]int `json:"byTier,omitempty"`

	Operation string                 `json:"operation"`
	Requests  []InflightRequestEntry `json:"requests,omitempty"`
	Request   *InflightRequestEntry  `json:"request,omitempty"`
	ID        string                 `json:"id,omitempty"`
}

func (e InFlightRequestsEvent) Type() uint32 {
	return InFlightRequestsEventID
}

type InflightRequestEntry struct {
	ID          string            `json:"id"`
	Timestamp   time.Time         `json:"timestamp"`
	Model       string            `json:"model"`
	ReqPath     string            `json:"req_path"`
	Method      string            `json:"method"`
	ReqHeaders  map[string]string `json:"req_headers"`
	RemoteIP    string            `json:"remote_ip"`
	RespHeaders map[string]string `json:"resp_headers"`
	RespBytes   int64             `json:"resp_bytes"`
	// RespTokens counts REAL model output on an Anthropic (/v1/messages)
	// stream, immune to the keepalive PING bytes that inflate RespBytes: it
	// increments once per `event: content_block_delta` SSE event and never for
	// an `event: ping`. A slot that only gets pinged stays at 0; a slot that
	// produced output then went flat freezes here while RespBytes keeps
	// climbing - the "0 real tokens vs healthy occupancy" signal the box was
	// blind to (llama-cm fixture hass-token-blind-2026-09-03). Non-Anthropic
	// (e.g. OpenAI) streams leave this at 0 in phase 1.
	RespTokens int64             `json:"resp_tokens"`
	ElapsedMs  int64             `json:"elapsed_ms"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

// ModelCapacity is one model's serving-slot occupancy: how many requests hold
// a slot right now, the ceiling, and how many are parked waiting for one.
//
// Granted is the count that matters for admission decisions. It is NOT the same
// as the number of tracked in-flight requests, which counts parked and running
// alike and therefore reads high while nothing is being served. A consumer
// deciding whether a NEW request can be served now wants Granted >= Limit.
type ModelCapacity struct {
	Model   string `json:"model"`
	Granted int    `json:"granted"`
	Limit   int    `json:"limit"`
	Queued  int    `json:"queued"`
}

// Cooldown is the ONE swap-grace state a box can be in: the resident
// EvicteeModel is idle but still inside its configured grace
// (config.ModelConfig.SwapGraceSeconds), and at least one queued request for
// another model is waiting for that grace to end before the swap proceeds.
// It is a property of the resident, not of each model queued behind it: two
// models waiting behind one cooling resident is still one cooldown
// (2026-09-10: two "waiting for cq35" rows for one resident).
//
// The grace exists to keep the resident's slots and their KV caches hot for
// a session that paused (tool call, AskUserQuestion), so Slots lists the
// resident's slots with the session each one is being kept warm for.
//
// RemainingSeconds counts down to the swap; it never counts up. NextModel is
// the oldest queued cross-model request's model - the one that loads when
// the cooldown ends. Waiting counts every queued request parked behind the
// cooldown, whatever model each asked for.
type Cooldown struct {
	EvicteeModel     string    `json:"evicteeModel"`
	NextModel        string    `json:"nextModel"`
	Waiting          int       `json:"waiting"`
	RemainingSeconds int       `json:"remainingSeconds"`
	Slots            []HotSlot `json:"slots"`
}

// HotSlot is one slot of the cooling resident and the session it last
// served (empty SessionID: no session lane owns it). IdleSeconds is how long
// ago that session was last seen on it.
type HotSlot struct {
	Slot        int    `json:"slot"`
	SessionID   string `json:"sessionId"`
	IdleSeconds int    `json:"idleSeconds"`
}

type ProfileChangedEvent struct {
	Active string
}

func (e ProfileChangedEvent) Type() uint32 {
	return ProfileChangedEventID
}
