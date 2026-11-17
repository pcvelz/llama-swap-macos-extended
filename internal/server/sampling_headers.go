package server

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Per-request sampling headers: a client (llama-cm's dispatch knobs) sends
// X-Temperature / X-Presence-Penalty / X-Repetition-Penalty on an inference
// request, and this rewrites the JSON body with those values before the body
// reaches the child. The header is the transport; the body field is the
// contract - the child never sees the header as anything but its own request
// parameters.
//
//   - X-Temperature:        0 < v <= 2    -> "temperature"      (overrides a body value)
//   - X-Presence-Penalty:   -2 <= v <= 2  -> "presence_penalty" (fill-only)
//   - X-Repetition-Penalty: 0 < v <= 3    -> the engine's own name (fill-only):
//     "repeat_penalty" for llama.cpp, "repetition_penalty" for Splash, both for `both`
//
// A header that is unparsable, NaN, or outside its range is ignored; a header
// NEVER fails a request. Only JSON POST bodies on the three completion
// endpoints are touched - everything else passes through.
//
// Engine scope: the repetition field is spelled differently per engine and the
// fork carries no general engine marker (a Splash child is only ever marked by
// its cmd), so the gate is the per-model opt-in key `samplingHeaders`, read via
// samplingModeOf: `true` or `llamacpp` -> llama.cpp's "repeat_penalty";
// `splash` -> Splash's "repetition_penalty" (Splaude 1.8.0 reads sampling from
// the Messages request body and has no header options of its own, so this
// translation is the only place the headers become sampling); `both` writes
// the two (a model whose engine is switched at runtime, e.g. cq27). A model
// that does not set the key is never rewritten.
//
// Order in the filter middleware: the headers fold in BEFORE the configured
// filters. A static default in `setParams` spelled `key?` (set-if-undefined)
// then fills only what the header and the client left out, so the header wins
// over the static default and the fill-only penalties keep a client's own value.
const (
	samplingTemperatureHeader   = "X-Temperature"
	samplingPresenceHeader      = "X-Presence-Penalty"
	samplingRepetitionHeader    = "X-Repetition-Penalty"
	samplingHeadersConfigKey    = "samplingHeaders"
	samplingHeadersMinExclusive = 0.0 // temperature and repetition penalty must be strictly above this
)

// samplingEndpoints are the endpoints whose JSON request bodies carry
// sampling parameters. Other endpoints (embeddings, rerank, responses,
// count_tokens, ...) are left untouched.
var samplingEndpoints = map[string]bool{
	"/v1/chat/completions": true,
	"/v1/completions":      true,
	"/v1/messages":         true,
}

// samplingMode is the engine dialect a model's sampling headers are written in.
type samplingMode int

const (
	samplingOff      samplingMode = iota
	samplingLlamaCpp              // repeat_penalty
	samplingSplash                // repetition_penalty
	samplingBoth                  // repeat_penalty and repetition_penalty
)

// samplingModeOf reads the per-model opt-in through the existing per-model
// Metadata bag (config.ModelConfig.Metadata, #264):
//
//	models:
//	  my-model:
//	    metadata:
//	      samplingHeaders: splash    # or true / llamacpp / both
//
// Absent, false or unknown = samplingOff: nothing is rewritten until a model asks.
func samplingModeOf(mc config.ModelConfig) samplingMode {
	switch v := mc.Metadata[samplingHeadersConfigKey].(type) {
	case bool:
		if v {
			return samplingLlamaCpp
		}
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true", "llamacpp", "llama.cpp":
			return samplingLlamaCpp
		case "splash":
			return samplingSplash
		case "both":
			return samplingBoth
		}
	}
	return samplingOff
}

// modelWantsSamplingHeaders reports whether the resolved model opted in at all.
func modelWantsSamplingHeaders(mc config.ModelConfig) bool {
	return samplingModeOf(mc) != samplingOff
}

// repetitionKeys is the body field(s) the repetition header lands in per mode.
func repetitionKeys(m samplingMode) []string {
	switch m {
	case samplingSplash:
		return []string{"repetition_penalty"}
	case samplingBoth:
		return []string{"repeat_penalty", "repetition_penalty"}
	default:
		return []string{"repeat_penalty"}
	}
}

// parseSamplingHeaderValue parses one header value: a plain float, finite,
// inside (min, max] when minInclusive is false, or [min, max] when true. ok is
// false for anything else (empty, garbage, NaN, Inf, out of range) - the
// caller ignores the header and the request proceeds unchanged.
func parseSamplingHeaderValue(h http.Header, name string, min, max float64, minInclusive bool) (float64, bool) {
	raw := h.Get(name)
	if raw == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	if v > max {
		return 0, false
	}
	if minInclusive {
		if v < min {
			return 0, false
		}
	} else if v <= min {
		return 0, false
	}
	return v, true
}

// applySamplingHeadersForModel is the whole rule, testable on its own: given
// the requested model id (as the body / dispatch names it), the request
// headers and the JSON body, it returns the rewritten body. Gates, in order:
// the model resolves to a local model, that model opted in via samplingHeaders,
// the body is valid JSON. Then each header independently: parse, range-check,
// set. temperature overrides an existing body value; the two penalties fill
// only (a body that already carries them keeps its own value). A gate that
// fails returns the body unchanged - a header never fails a request.
func applySamplingHeadersForModel(cfg config.Config, requested string, h http.Header, body []byte) ([]byte, error) {
	if requested == "" || len(body) == 0 {
		return body, nil
	}
	realName, found := cfg.RealModelName(requested)
	if !found {
		return body, nil
	}
	mc, ok := cfg.Models[realName]
	mode := samplingModeOf(mc)
	if !ok || mode == samplingOff {
		return body, nil
	}
	if !gjson.ValidBytes(body) {
		return body, nil
	}

	var err error

	if v, ok := parseSamplingHeaderValue(h, samplingTemperatureHeader, samplingHeadersMinExclusive, 2, false); ok {
		if body, err = sjson.SetBytes(body, "temperature", v); err != nil {
			return nil, fmt.Errorf("error setting temperature from %s: %w", samplingTemperatureHeader, err)
		}
	}
	if v, ok := parseSamplingHeaderValue(h, samplingPresenceHeader, -2, 2, true); ok {
		if !gjson.GetBytes(body, "presence_penalty").Exists() {
			if body, err = sjson.SetBytes(body, "presence_penalty", v); err != nil {
				return nil, fmt.Errorf("error setting presence_penalty from %s: %w", samplingPresenceHeader, err)
			}
		}
	}
	if v, ok := parseSamplingHeaderValue(h, samplingRepetitionHeader, samplingHeadersMinExclusive, 3, false); ok {
		for _, key := range repetitionKeys(mode) {
			if gjson.GetBytes(body, key).Exists() {
				continue
			}
			if body, err = sjson.SetBytes(body, key, v); err != nil {
				return nil, fmt.Errorf("error setting %s from %s: %w", key, samplingRepetitionHeader, err)
			}
		}
	}
	return body, nil
}
