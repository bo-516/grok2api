package server

import (
	"context"
	"crypto/subtle"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/shaoboli/agent-mock/internal/config"
	"github.com/shaoboli/agent-mock/internal/grok"
)

// maxBody is the largest accepted request body. Bigger bodies are 413.
const maxBody = 8 << 20

// logKey is the context key for the per-request access record.
type logKey struct{}

// logRec is filled by handlers and written once by the log middleware.
type logRec struct {
	// status is the HTTP status. 0 means 200.
	status int
	// reqModel is the caller model. usedModel is the grok model.
	reqModel, usedModel string
	// stream is the request stream flag.
	stream bool
	// tools is the number of tools on the request.
	tools int
	// inTok and outTok are prompt and completion tokens. Zero until a run finishes.
	inTok, outTok int
	// prompt is included only when log prompts is on.
	prompt string
	// note is a short marker such as system_file=1. It is not the prompt body.
	note string
	// chat marks a /v1/chat/completions request so the log line uses the chat shape.
	chat bool
}

// Server is the HTTP API. Runner executes grok. Known is the model list from startup.
type Server struct {
	// Config is the process configuration.
	Config config.Config
	// Runner executes restricted grok runs. It must be non-nil.
	Runner *grok.Runner
	// Known is the grok model ids from `grok models`. Used for passthrough.
	Known []string
	// DefaultModel is grok's own default, used only as the response model when
	// neither the request nor Config.DefaultModel selected one and grok did not say.
	DefaultModel string
	// Login is "ok" or "not_logged_in".
	Login string
	// GrokVersion is the probed grok version, such as 1.0.41.
	GrokVersion string
	// Keepalive is the SSE comment interval. Zero uses 15s.
	Keepalive time.Duration
	// Log receives one access line per request. Nil discards logs.
	Log io.Writer
	// Now is the clock for the created field. Nil uses time.Now.
	Now func() time.Time

	lim *limiter
}

// New builds a server. Keepalive defaults to 15s. The limiter uses Config.
func New(cfg config.Config, runner *grok.Runner) *Server {
	return &Server{
		Config:    cfg,
		Runner:    runner,
		Login:     "not_logged_in",
		Keepalive: 15 * time.Second,
		lim:       newLimiter(cfg.MaxConcurrency, cfg.QueueTimeout),
	}
}

// Handler is the routed HTTP handler with auth, one-line access logs, and panic recovery.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.chat)
	mux.HandleFunc("GET /v1/models", s.models)
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /doc", s.doc)
	return s.recover(s.logWrap(s.auth(mux)))
}

// auth requires Authorization: Bearer when Config.APIKey is set.
// GET /doc is exempt so an agent can read the guide before it has a token.
// A missing or wrong token is 401 invalid_api_key. Loopback with an empty key skips this.
func (s *Server) auth(next http.Handler) http.Handler {
	if s.Config.APIKey == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/doc" {
			next.ServeHTTP(w, r)
			return
		}
		if !bearerOK(r.Header.Get("Authorization"), s.Config.APIKey) {
			writeAPI(w, http.StatusUnauthorized, "authentication_error", "invalid_api_key", "Invalid API key. Send Authorization: Bearer <key>.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bearerOK compares the Authorization header to "Bearer "+key in constant time.
// A missing prefix or a different length is a mismatch. The key itself is not logged.
func bearerOK(header, key string) bool {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	got := header[len(prefix):]
	if len(got) != len(key) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(key)) == 1
}

// logWrap writes exactly one access line after the handler returns.
func (s *Server) logWrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &logRec{}
		ww := &statusWriter{ResponseWriter: w, rec: rec}
		start := time.Now()
		next.ServeHTTP(ww, r.WithContext(context.WithValue(r.Context(), logKey{}, rec)))
		if rec.status == 0 {
			if ww.code != 0 {
				rec.status = ww.code
			} else {
				rec.status = http.StatusOK
			}
		}
		s.writeLog(r, rec, time.Since(start))
	})
}

// writeLog formats the single access line. Prompt text is appended only when rec.prompt is set,
// which the chat handler does only if -log-prompts is on.
func (s *Server) writeLog(r *http.Request, rec *logRec, dur time.Duration) {
	if s.Log == nil {
		return
	}
	model := "-"
	if rec.chat {
		reqModel := rec.reqModel
		if reqModel == "" {
			reqModel = "default"
		}
		model = reqModel + "->" + rec.usedModel
	}
	line := fmt.Sprintf("%s %s %s model=%s stream=%t tools=%d status=%d dur=%.1fs in=%d out=%d",
		time.Now().Format("15:04:05"), r.Method, r.URL.Path, model, rec.stream, rec.tools, rec.status, dur.Seconds(), rec.inTok, rec.outTok)
	if rec.note != "" {
		line += " " + rec.note
	}
	if rec.prompt != "" {
		line += " prompt=" + fmt.Sprintf("%q", strings.ReplaceAll(rec.prompt, "\n", " "))
	}
	fmt.Fprintln(s.Log, line)
}

// recover turns a panic into a 500 body when the handler has not written yet.
func (s *Server) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("panic: %v", rec)
				writeAPI(w, http.StatusInternalServerError, "server_error", "internal_error", "agent-mock panicked while handling the request.")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// health writes the liveness JSON. login is ok or not_logged_in.
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	login := s.Login
	if login == "" {
		login = "not_logged_in"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":          "ok",
		"grok":            s.GrokVersion,
		"login":           login,
		"inflight":        s.lim.Inflight(),
		"max_concurrency": s.Config.MaxConcurrency,
	})
}

// models lists superllm first, then grok models (owned_by xai) and -model-map aliases (owned_by agent-mock-alias).
func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	type item struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int    `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	var data []item
	seen := map[string]bool{publicModel: true}
	data = append(data, item{ID: publicModel, Object: "model", OwnedBy: publicModel})
	for _, id := range s.Known {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		data = append(data, item{ID: id, Object: "model", OwnedBy: "xai"})
	}
	for alias := range s.Config.ModelMap {
		if seen[alias] {
			continue
		}
		seen[alias] = true
		data = append(data, item{ID: alias, Object: "model", OwnedBy: "agent-mock-alias"})
	}
	if data == nil {
		data = []item{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// statusWriter records the status code and forwards Flush/Unwrap so SSE still flushes.
type statusWriter struct {
	http.ResponseWriter
	code int
	rec  *logRec
}

// WriteHeader records the status and forwards it. The first call wins for the log.
func (s *statusWriter) WriteHeader(code int) {
	if s.code == 0 {
		s.code = code
		if s.rec != nil && s.rec.status == 0 {
			s.rec.status = code
		}
	}
	s.ResponseWriter.WriteHeader(code)
}

// Write sends bytes and implies 200 when the handler never called WriteHeader.
func (s *statusWriter) Write(p []byte) (int, error) {
	if s.code == 0 {
		s.WriteHeader(http.StatusOK)
	}
	return s.ResponseWriter.Write(p)
}

// Flush flushes the underlying writer when it supports it.
func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the real writer.
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// recFrom returns the access record, or a discarded one when the handler is called bare.
func recFrom(ctx context.Context) *logRec {
	if v, ok := ctx.Value(logKey{}).(*logRec); ok {
		return v
	}
	return &logRec{}
}

// now returns the request clock.
func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// keepalive returns the SSE idle interval.
func (s *Server) keepalive() time.Duration {
	if s.Keepalive > 0 {
		return s.Keepalive
	}
	return 15 * time.Second
}

// ignoredHeader joins inert field names. An empty list returns "" so the header is omitted.
func ignoredHeader(names []string) string {
	return strings.Join(names, ",")
}
