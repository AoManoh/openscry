package mcpserver

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// maxHTTPBodyBytes caps a single JSON-RPC POST body, a defensive bound against
// pathologically large requests.
const maxHTTPBodyBytes = 4 << 20 // 4 MiB

// defaultHTTPRequestTimeout bounds one /mcp request end-to-end. It is generous
// because a web_search_batch can legitimately run several sequential waves of
// upstream searches; the per-operation timeouts in the resilience layer bound
// each underlying call.
const defaultHTTPRequestTimeout = 10 * time.Minute

// HTTPOptions configures the HTTP JSON-RPC transport.
type HTTPOptions struct {
	Addr           string                          // listen address, e.g. ":8080"
	APIKey         string                          // required bearer token; empty => refuse to start
	RequestTimeout time.Duration                   // per-request budget; <= 0 uses the default
	ReadinessProbe func(ctx context.Context) error // optional /ready upstream check; nil = liveness only
	ConfigInfo     map[string]any                  // static payload for /.well-known/mcp-config
	// MaxInFlight bounds concurrently-processing /mcp requests. net/http
	// spawns a goroutine per connection, so without this an HTTP request
	// flood is unbounded (goroutines, FDs, memory) — unlike stdio, whose
	// engine worker pool already bounds processing. This admission limit is
	// the HTTP analog. It is orthogonal to the upstream-concurrency limiter
	// (which bounds grok2api load): this protects local resources. <= 0
	// disables admission control.
	MaxInFlight int
}

// ServeHTTP runs the MCP server over a minimal HTTP JSON-RPC transport until
// ctx is cancelled. Each POST /mcp carries one JSON-RPC message dispatched
// through the same protocolHandler as the stdio path; the reply is a single
// application/json response (202 Accepted for notifications, which have no
// response). Infrastructure endpoints (/health, /ready, /.well-known/
// mcp-config) are unauthenticated by convention; only /mcp requires the key.
//
// Auth is mandatory: an empty APIKey is a configuration error. We never expose
// an unauthenticated tool-invocation endpoint on the network.
func (s *Server) ServeHTTP(ctx context.Context, opt HTTPOptions) error {
	mux, err := s.httpMux(opt)
	if err != nil {
		return err
	}

	httpSrv := &http.Server{
		Addr:              opt.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: a long batch request must not be cut off mid-flight;
		// the per-request ctx (reqTimeout) and resilience timeouts bound work.
	}

	errCh := make(chan error, 1)
	go func() {
		s.logger.Info("openscry.http_serving", "addr", opt.Addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

// httpMux builds the HTTP route table: unauthenticated infra endpoints
// (/health, /ready, /.well-known/mcp-config) plus the authenticated /mcp
// JSON-RPC endpoint. It returns an error when the mandatory API key is absent,
// so ServeHTTP fails loud rather than exposing an open endpoint.
func (s *Server) httpMux(opt HTTPOptions) (http.Handler, error) {
	if strings.TrimSpace(opt.APIKey) == "" {
		return nil, errors.New("http transport: API key required (set GROK_HTTP_API_KEY); refusing to serve an unauthenticated /mcp endpoint")
	}
	reqTimeout := opt.RequestTimeout
	if reqTimeout <= 0 {
		reqTimeout = defaultHTTPRequestTimeout
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})
	mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		if opt.ReadinessProbe != nil {
			if err := opt.ReadinessProbe(r.Context()); err != nil {
				writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "not_ready", "error": err.Error()})
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
	})
	mux.HandleFunc("/.well-known/mcp-config", func(w http.ResponseWriter, r *http.Request) {
		info := opt.ConfigInfo
		if info == nil {
			info = map[string]any{}
		}
		writeJSON(w, http.StatusOK, info)
	})
	// Admission control wraps the handler INSIDE auth: an unauthenticated
	// flood is rejected by requireAuth (a cheap constant-time compare) before
	// it can consume an admission slot, so it cannot starve authenticated
	// clients.
	var admit chan struct{}
	if opt.MaxInFlight > 0 {
		admit = make(chan struct{}, opt.MaxInFlight)
	}
	mux.Handle("/mcp", s.requireAuth(opt.APIKey, s.admitInFlight(admit, s.mcpPostHandler(reqTimeout))))
	return mux, nil
}

// admitInFlight bounds concurrently-processing /mcp requests with a buffered
// channel. Acquisition is non-blocking (try-acquire): when the limiter is full
// the request is rejected immediately with 503 + Retry-After rather than
// queued, because an HTTP client can retry on its own — this is the
// transport-appropriate backpressure, distinct from stdio's never-drop
// synchronous fallback. A nil semaphore disables admission control.
func (s *Server) admitInFlight(sem chan struct{}, next http.Handler) http.Handler {
	if sem == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
			next.ServeHTTP(w, r)
		default:
			w.Header().Set("Retry-After", "1")
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "server at capacity; retry shortly"})
		}
	})
}

// mcpPostHandler dispatches one JSON-RPC message per POST through the shared
// protocolHandler.
func (s *Server) mcpPostHandler(reqTimeout time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed; POST a single JSON-RPC message"})
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxHTTPBodyBytes))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "read body: " + err.Error()})
			return
		}

		ctx := r.Context()
		if reqTimeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, reqTimeout)
			defer cancel()
		}

		resp, herr := s.handler.HandleMessage(ctx, body)
		if herr != nil {
			// The handler only returns an error to suppress the response
			// (request ctx cancelled, e.g. client disconnect). Nothing to send.
			return
		}
		if resp == nil {
			// A notification (no id): accepted, no response body.
			w.WriteHeader(http.StatusAccepted)
			return
		}
		out, mErr := json.Marshal(resp)
		if mErr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "marshal response: " + mErr.Error()})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out)
	})
}

// requireAuth wraps next with bearer/X-API-Key validation using a constant-time
// comparison so a wrong key cannot be distinguished by timing.
func (s *Server) requireAuth(key string, next http.Handler) http.Handler {
	want := []byte(key)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(extractToken(r))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="openscry"`)
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized: present the API key via Authorization: Bearer or X-API-Key"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// extractToken pulls the credential from either the Authorization: Bearer
// header or the X-API-Key header.
func extractToken(r *http.Request) string {
	if h := strings.TrimSpace(r.Header.Get("Authorization")); h != "" {
		if rest, ok := strings.CutPrefix(h, "Bearer "); ok {
			return strings.TrimSpace(rest)
		}
		return h // tolerate a bare token in the Authorization header
	}
	return strings.TrimSpace(r.Header.Get("X-API-Key"))
}

// writeJSON writes v as a JSON response with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
