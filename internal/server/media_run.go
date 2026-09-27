package server

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/shaoboli/agent-mock/internal/grok"
	"github.com/shaoboli/agent-mock/internal/media"
	"github.com/shaoboli/agent-mock/internal/openai"
)

// readMedia reads a media body up to 64 MiB.
// A larger body is 413 request_too_large and ok is false. The handler has
// already written the error. A short read is 400 invalid_json.
func (s *Server) readMedia(w http.ResponseWriter, r *http.Request, rec *logRec) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxMediaBody))
	if err != nil {
		if openai.TooLarge(err) {
			rec.status = http.StatusRequestEntityTooLarge
			writeAPI(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request_too_large", "request body exceeds 64 MiB")
			return nil, false
		}
		rec.status = http.StatusBadRequest
		writeAPI(w, http.StatusBadRequest, "invalid_request_error", "invalid_json", "could not read the request body")
		return nil, false
	}
	return body, true
}

// mediaOff writes 404 media_disabled when -media is false.
// It returns true when the handler should stop. The response names the flag.
func (s *Server) mediaOff(w http.ResponseWriter, rec *logRec) bool {
	if s.Config.Media {
		return false
	}
	rec.status = http.StatusNotFound
	rec.kind = "media"
	writeAPI(w, http.StatusNotFound, "invalid_request_error", "media_disabled", "media routes are off (-media=false)")
	return true
}

// toolReady reports whether name was offered at startup.
// A nil MediaTools allows the tool. A missing tool writes 403 and returns false
// without starting grok.
func (s *Server) toolReady(w http.ResponseWriter, rec *logRec, name string) bool {
	if s.MediaTools == nil {
		return true
	}
	for _, t := range s.MediaTools {
		if t == name {
			return true
		}
	}
	rec.status = http.StatusForbidden
	writeAPI(w, http.StatusForbidden, "permission_error", grok.CodeMediaUnavailable, "grok media tool "+name+" is not available")
	return false
}

// sessionsRoot is the directory grok stores this child's session files in.
// It follows GROK_HOME from the same environment the child receives.
func (s *Server) sessionsRoot() string {
	parent := os.Environ()
	if s.Runner != nil && s.Runner.Environ != nil {
		parent = s.Runner.Environ()
	}
	home := grok.GrokHome(grok.ChildEnv(parent))
	if home == "" {
		return ""
	}
	return filepath.Join(home, "sessions")
}

// mcwd is the media working directory, separate from the chat cwd.
func (s *Server) mcwd() string {
	if s.Runner == nil {
		return ""
	}
	return filepath.Join(s.Runner.RootDir(), "mcwd")
}

// runPlan executes one media grok run. The guard copies outputs before the
// session is deleted. ErrStop from a complete plan comes back as a nil error.
// The returned items are empty when the guard discarded them.
func (s *Server) runPlan(ctx context.Context, plan media.Plan, g *media.Guard, onUse func(grok.ToolUse) error) (grok.Final, error) {
	return s.Runner.Run(ctx, plan.Spec, grok.Events{
		OnInit: func(info grok.Init) error {
			g.SetSession(firstID(info.SessionID, plan.Spec.SessionID))
			return nil
		},
		OnToolUse: func(tu grok.ToolUse) error {
			if onUse != nil {
				if err := onUse(tu); err != nil {
					return err
				}
			}
			return g.OnToolUse(tu)
		},
		OnToolResult: g.OnToolResult,
	})
}

// finishImage writes the ImagesResponse or a media error.
// Fewer files than planned is 502 grok_media_failed and those files are removed.
// format is url or b64_json. origin is scheme://host with no path.
func (s *Server) finishImage(w http.ResponseWriter, r *http.Request, rec *logRec, plan media.Plan, g *media.Guard, final grok.Final, runErr error, format string, ignored []string) {
	rec.inTok = final.Usage.PromptTokens()
	rec.outTok = final.Usage.CompletionTokens()
	if g.Rewritten() {
		if rec.note != "" {
			rec.note += " "
		}
		rec.note += "prompt_rewritten=1"
	}
	items, prompts := g.Outputs()
	if ge, ok := grok.AsError(runErr); ok && ge.Code == grok.CodeMaxTurns && len(items) == len(plan.Calls) {
		runErr = nil
	}
	if runErr != nil || len(items) != len(plan.Calls) {
		g.Discard()
		rec.files = 0
		if runErr == nil {
			rec.status = http.StatusBadGateway
			writeAPI(w, http.StatusBadGateway, "api_error", grok.CodeMediaFailed, fmt.Sprintf("got %d of %d %s", len(items), len(plan.Calls), plan.Noun))
			return
		}
		if errors.Is(runErr, context.Canceled) {
			return
		}
		rec.status = writeRunError(w, runErr)
		return
	}
	rec.files = len(items)
	setRunHeaders(w.Header(), final.SessionID, ignored)
	data := make([]map[string]string, 0, len(items))
	formatName := ""
	for i, it := range items {
		prompt := ""
		if i < len(prompts) {
			prompt = prompts[i]
		}
		row := map[string]string{"revised_prompt": prompt}
		if format == "b64_json" {
			b, err := os.ReadFile(it.Path)
			if err != nil {
				g.Discard()
				rec.files = 0
				rec.status = http.StatusBadGateway
				writeAPI(w, http.StatusBadGateway, "api_error", grok.CodeMediaFailed, "could not read the stored image")
				return
			}
			row["b64_json"] = base64.StdEncoding.EncodeToString(b)
		} else {
			row["url"] = requestOrigin(r) + "/v1/media/" + it.Name
		}
		data = append(data, row)
		if formatName == "" {
			formatName = formatOf(it.Type)
		}
	}
	body := map[string]any{"created": s.now().Unix(), "data": data}
	if formatName != "" {
		body["output_format"] = formatName
	}
	writeJSON(w, http.StatusOK, body)
}

// formatOf maps a sniffed content type to the OpenAI output_format value.
func formatOf(ctype string) string {
	switch ctype {
	case "image/png":
		return "png"
	case "image/webp":
		return "webp"
	case "image/jpeg":
		return "jpeg"
	default:
		return ""
	}
}

// firstID returns the first non-empty session id.
func firstID(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// mediaCtx is the image request deadline. A zero RequestTimeout uses 3 minutes
// so a test that forgot the field does not cancel immediately.
func (s *Server) mediaCtx(r *http.Request) (context.Context, context.CancelFunc) {
	d := s.Config.RequestTimeout
	if d <= 0 {
		d = 3 * time.Minute
	}
	return context.WithTimeout(r.Context(), d)
}
