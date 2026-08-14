package server

import (
	"bytes"
	"io"
	"net/http"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/chain"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// CreateMetricsMiddleware returns middleware that records token metrics for
// model-dispatched POST requests. It resolves the model, tees the response into
// a buffer, and parses token usage once the upstream handler returns.
func CreateMetricsMiddleware(mm *metricsMonitor, cfg config.Config) chain.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if mm == nil || r.Method != http.MethodPost {
				next.ServeHTTP(w, r)
				return
			}

			// Determine the model-routed endpoint path. Regular routes are
			// already meterable; /upstream/<model>/<path> is metered only when
			// the remaining path matches a model-dispatched endpoint.
			checkPath := r.URL.Path
			if strings.HasPrefix(r.URL.Path, "/upstream/") {
				var found bool
				_, _, checkPath, found = swaputil.FindModelInPath(cfg, strings.TrimPrefix(r.URL.Path, "/upstream"))
				if !found {
					next.ServeHTTP(w, r)
					return
				}
			}

			if !isMetricsRecordPath(checkPath) {
				next.ServeHTTP(w, r)
				return
			}

			// Resolve the model now so downstream dispatch hits the context
			// fast path; FetchContext restores the request body for regular
			// routes and extracts the model from the URL for /upstream routes.
			data, err := swaputil.FetchContext(r, cfg)
			if err != nil {
				swaputil.SendError(w, r, swaputil.ErrNoModelInContext)
				return
			}

			// Capture fields for this route, and (if captures are enabled) a
			// pre-compressed request capture handed off before dispatch.
			//
			// On the regular model routes the body is already buffered on the
			// shared context by FetchContext/filters.go, so no separate
			// io.ReadAll is needed. What used to be a raw reqBody []byte
			// pinned for the entire streamed-response duration (which can be
			// hours while a request sits parked in llama-swap's queue) is
			// instead compressed immediately into the same zstd+CBOR capture
			// format used for final storage. Only the much smaller compressed
			// blob survives until record() below merges it with the response
			// capture.
			cf := captureFieldsFor(checkPath)
			var reqCapture []byte
			if mm.enableCaptures && cf&(captureReqBody|captureReqHeaders) != 0 {
				partial := ReqRespCapture{ReqPath: r.URL.Path}
				if cf&captureReqBody != 0 {
					// /upstream routes resolve the model from the URL, so
					// extractUpstreamContext never reads (and never buffers)
					// the body - fall back to a single read + restore there,
					// otherwise those captures would silently store no
					// request body at all.
					partial.ReqBody = data.Body
					if partial.ReqBody == nil && r.Body != nil {
						if buffered, err := io.ReadAll(r.Body); err == nil {
							r.Body.Close()
							r.Body = io.NopCloser(bytes.NewReader(buffered))
							partial.ReqBody = buffered
						}
					}
				}
				if cf&captureReqHeaders != 0 {
					headers := headerMap(r.Header)
					redactHeaders(headers)
					partial.ReqHeaders = headers
				}
				if len(partial.ReqBody) > 0 || len(partial.ReqHeaders) > 0 {
					if compressed, _, err := compressCapture(&partial); err == nil {
						reqCapture = compressed
					} else {
						mm.logger.Warnf("failed to pre-compress request capture: %v, path=%s", err, r.URL.Path)
					}
				}
			}

			// Restrict Accept-Encoding to encodings we can decompress so the
			// buffered response body stays parseable.
			if ae := r.Header.Get("Accept-Encoding"); ae != "" {
				r.Header.Set("Accept-Encoding", filterAcceptEncoding(ae))
			}

			recorder := newBodyCopier(w)
			next.ServeHTTP(recorder, r)
			mm.record(data.ModelID, r, recorder, cf, reqCapture)
		})
	}
}
