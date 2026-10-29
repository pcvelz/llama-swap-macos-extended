package server

import (
	"fmt"
	"math"
	"net/http"
	"strconv"

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
//   - X-Repetition-Penalty: 0 < v <= 3    -> "repeat_penalty"   (fill-only)
//
// A header that is unparsable, NaN, or outside its range is ignored; a header
// NEVER fails a request. Only JSON POST bodies on the three completion
// endpoints are touched - everything else passes through.
//
// Engine scope: the body fields named above are llama.cpp's. The fork carries
// no general engine marker (a Splash child is only ever marked by its cmd), so
// the gate is the per-model opt-in key `samplingHeaders: true`, read here via
// modelWantsSamplingHeaders. It is enabled in no shipped config yet; a model
// that does not set the key is never rewritten.
const (
	samplingTemperatureHeader   = "X-Temperature"
	samplingPresenceHeader      = "X-Presence-Penalty"
	samplingRepetitionHeader    = "X-Repetition-Penalty"
	samplingHeadersConfigKey    = "samplingHeaders"
	samplingHeadersMinExclusive = 0.0 // temperature and repeat_penalty must be strictly above this
)

// samplingEndpoints are the endpoints whose JSON request bodies carry llama.cpp
// sampling parameters. Other endpoints (embeddings, rerank, responses,
// count_tokens, ...) are left untouched.
var samplingEndpoints = map[string]bool{
	"/v1/chat/completions": true,
	"/v1/completions":      true,
	"/v1/messages":         true,
}

// modelWantsSamplingHeaders reports whether the resolved model opted in. The
// fork has no engine marker to consult, so the opt-in is the per-model config
// key `samplingHeaders: true` (see the header comment), read through the
// existing per-model Metadata bag (config.ModelConfig.Metadata, #264):
//
//	models:
//	  my-model:
//	    metadata:
//	      samplingHeaders: true
//
// Absent or non-true = false: nothing is rewritten until a model asks.
func modelWantsSamplingHeaders(mc config.ModelConfig) bool {
	v, ok := mc.Metadata[samplingHeadersConfigKey]
	if !ok {
		return false
	}
	b, ok := v.(bool)
	return ok && b
}

// parseSamplingHeaderValue parses one header value: a plain float, finite,
// inside (minExclusive, max] when minExclusive is the open bound, or
// [min, max] when min == minInclusiveAny. ok is false for anything else
// (empty, garbage, NaN, Inf, out of range) - the caller ignores the header and
// the request proceeds unchanged.
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
	if !ok || !modelWantsSamplingHeaders(mc) {
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
		if !gjson.GetBytes(body, "repeat_penalty").Exists() {
			if body, err = sjson.SetBytes(body, "repeat_penalty", v); err != nil {
				return nil, fmt.Errorf("error setting repeat_penalty from %s: %w", samplingRepetitionHeader, err)
			}
		}
	}
	return body, nil
}
