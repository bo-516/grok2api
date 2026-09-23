package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/shaoboli/agent-mock/internal/grok"
	"github.com/shaoboli/agent-mock/internal/openai"
	"github.com/shaoboli/agent-mock/internal/prompt"
)

// chat is POST /v1/chat/completions. It validates, waits for a grok slot, renders
// a prompt, runs one restricted grok process, and writes JSON or SSE.
func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	rec := recFrom(r.Context())
	rec.chat = true
	created := s.now().Unix()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			rec.status = http.StatusRequestEntityTooLarge
			writeAPI(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request_too_large", "request body exceeds 8 MiB")
			return
		}
		rec.status = http.StatusBadRequest
		writeAPI(w, http.StatusBadRequest, "invalid_request_error", "invalid_json", "could not read the request body")
		return
	}
	req, err := openai.Parse(body)
	if err != nil {
		var re *openai.RequestError
		if errors.As(err, &re) {
			rec.status = re.Status
			writeRequestError(w, re)
			return
		}
		rec.status = http.StatusBadRequest
		writeAPI(w, http.StatusBadRequest, "invalid_request_error", "invalid_json", err.Error())
		return
	}
	rec.reqModel = req.Model
	rec.stream = req.Stream
	rec.tools = len(req.Tools)
	if err := s.lim.Acquire(r.Context()); err != nil {
		if errors.Is(err, errBusy) {
			rec.status = http.StatusTooManyRequests
			w.Header().Set("Retry-After", "5")
			writeAPI(w, http.StatusTooManyRequests, "rate_limit_error", "agent_mock_busy", err.Error())
			return
		}
		return
	}
	defer s.lim.Release()

	rendered, err := prompt.Render(req)
	if err != nil {
		var re *openai.RequestError
		if errors.As(err, &re) {
			rec.status = re.Status
			writeRequestError(w, re)
			return
		}
		rec.status = http.StatusBadRequest
		writeAPI(w, http.StatusBadRequest, "invalid_request_error", "unsupported_parameter", err.Error())
		return
	}
	if rendered.SystemInFile {
		rec.note = "system_file=1"
	}
	if s.Config.LogPrompts {
		rec.prompt = rendered.Prompt
	}
	grokModel := resolveModel(req.Model, s.Known, s.Config.ModelMap, s.Config.DefaultModel)
	effort := req.ReasoningEffort
	if effort == "" {
		effort = s.Config.ReasoningEffort
	}
	spec := grok.RunSpec{
		Prompt:          rendered.Prompt,
		SystemOverride:  rendered.SystemOverride,
		Model:           grokModel,
		ReasoningEffort: effort,
		JSONSchema:      rendered.JSONSchema,
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.Config.RequestTimeout)
	defer cancel()

	if req.Stream && !rendered.Envelope {
		s.streamText(ctx, w, r, rec, created, req, rendered, spec)
		return
	}
	final, runErr := s.Runner.Run(ctx, spec, grok.Events{})
	s.noteUsage(rec, final)
	if runErr != nil {
		s.fail(w, rec, runErr, final.SessionID, req.Ignored)
		return
	}
	used := s.usedModel(final.Model, grokModel)
	rec.usedModel = used
	if rendered.Envelope {
		out, err := prompt.ParseOutcome(final.Structured, final.Text, req.Tools, rendered.MinCalls, rendered.MaxCalls)
		if err != nil {
			s.fail(w, rec, err, final.SessionID, req.Ignored)
			return
		}
		if req.Stream {
			s.pseudo(w, rec, created, final, used, req, out)
			return
		}
		s.writeOutcome(w, rec, created, final, used, req, out)
		return
	}
	text := final.Text
	if req.Stream {
		s.pseudo(w, rec, created, final, used, req, &prompt.Outcome{Reply: true, Content: text})
		return
	}
	s.writeText(w, rec, created, final, used, req.Ignored, text)
}

// streamText forwards text deltas as SSE. Headers are written on message_start.
// If grok returns a single JSON object with no message_start, the text is pseudo-streamed.
// An error after headers is one SSE error event and the connection closes without [DONE].
func (s *Server) streamText(ctx context.Context, w http.ResponseWriter, r *http.Request, rec *logRec, created int64, req *openai.Request, rendered *prompt.Rendered, spec grok.RunSpec) {
	sw := newSSE(w, s.keepalive())
	defer sw.Close()
	var session, reported string
	id := ""
	final, runErr := s.Runner.Run(ctx, spec, grok.Events{
		OnInit: func(info grok.Init) error {
			if info.SessionID != "" {
				session = info.SessionID
			}
			if info.Model != "" {
				reported = info.Model
			}
			return nil
		},
		OnMessageStart: func(sid, model string) error {
			if sid != "" {
				session = sid
			}
			if model != "" {
				reported = model
			}
			if id == "" {
				id = openai.CompletionID(session)
			}
			used := s.usedModel(reported, spec.Model)
			if err := sw.begin(func(h http.Header) { setRunHeaders(h, session, req.Ignored) }); err != nil {
				return err
			}
			return sw.Send(openai.RoleChunk(id, created, used))
		},
		OnText: func(delta string) error {
			if !sw.Started() {
				return nil
			}
			used := s.usedModel(reported, spec.Model)
			return sw.Send(openai.ContentChunk(id, created, used, delta))
		},
	})
	s.noteUsage(rec, final)
	used := s.usedModel(firstNonEmpty(final.Model, reported), spec.Model)
	rec.usedModel = used
	if session == "" {
		session = final.SessionID
	}
	if id == "" {
		id = openai.CompletionID(session)
	}
	if runErr != nil {
		if sw.Started() {
			s.sseError(sw, runErr)
			rec.status = http.StatusOK
			return
		}
		s.fail(w, rec, runErr, session, req.Ignored)
		return
	}
	if !sw.Started() {
		s.pseudo(w, rec, created, final, used, req, &prompt.Outcome{Reply: true, Content: final.Text})
		return
	}
	reason := openai.MapFinishReason(final.StopReason)
	_ = sw.Send(openai.FinishChunk(id, created, used, reason))
	if req.IncludeUsage {
		_ = sw.Send(openai.UsageChunk(id, created, used, usageOf(final)))
	}
	sw.Done()
	rec.status = http.StatusOK
}

// pseudo writes a short SSE stream from a finished run: role, then content or tool calls, then finish.
// Tool calls follow the OpenAI fragment shape: name first, then the arguments string.
// A legacy functions request uses function_call instead. include_usage adds the empty-choices chunk.
// The stream ends with [DONE].
func (s *Server) pseudo(w http.ResponseWriter, rec *logRec, created int64, final grok.Final, used string, req *openai.Request, out *prompt.Outcome) {
	sw := newSSE(w, 0)
	defer sw.Close()
	id := openai.CompletionID(final.SessionID)
	if err := sw.begin(func(h http.Header) { setRunHeaders(h, final.SessionID, req.Ignored) }); err != nil {
		return
	}
	if out != nil && !out.Reply && len(out.Calls) > 0 {
		var chunks []openai.Chunk
		if req.Legacy {
			chunks = openai.FunctionCallChunks(id, created, used, out.Content, out.Calls[0])
		} else {
			chunks = openai.ToolCallChunks(id, created, used, out.Content, out.Calls)
		}
		for _, ch := range chunks {
			_ = sw.Send(ch)
		}
	} else {
		_ = sw.Send(openai.RoleChunk(id, created, used))
		text := ""
		if out != nil {
			text = out.Content
		}
		if text != "" {
			_ = sw.Send(openai.ContentChunk(id, created, used, text))
		}
		_ = sw.Send(openai.FinishChunk(id, created, used, openai.MapFinishReason(final.StopReason)))
	}
	if req.IncludeUsage {
		_ = sw.Send(openai.UsageChunk(id, created, used, usageOf(final)))
	}
	sw.Done()
	rec.status = http.StatusOK
}

// writeText writes a non-streaming text completion.
func (s *Server) writeText(w http.ResponseWriter, rec *logRec, created int64, final grok.Final, used string, ignored []string, text string) {
	setRunHeaders(w.Header(), final.SessionID, ignored)
	body := openai.NewCompletion(openai.CompletionID(final.SessionID), created, used, openai.MapFinishReason(final.StopReason), &text, nil, usageOf(final))
	writeJSON(w, http.StatusOK, body)
	rec.status = http.StatusOK
}

// writeOutcome writes a non-streaming tool-call or envelope reply completion.
// A non-empty preamble is message.content next to tool_calls. Empty content stays null.
// Legacy requests write function_call and finish_reason function_call, and only the first call.
func (s *Server) writeOutcome(w http.ResponseWriter, rec *logRec, created int64, final grok.Final, used string, req *openai.Request, out *prompt.Outcome) {
	setRunHeaders(w.Header(), final.SessionID, req.Ignored)
	if out.Reply || len(out.Calls) == 0 {
		text := out.Content
		body := openai.NewCompletion(openai.CompletionID(final.SessionID), created, used, openai.MapFinishReason(final.StopReason), &text, nil, usageOf(final))
		writeJSON(w, http.StatusOK, body)
		rec.status = http.StatusOK
		return
	}
	var content *string
	if out.Content != "" {
		text := out.Content
		content = &text
	}
	calls := out.Calls
	finish := "tool_calls"
	if req.Legacy {
		finish = "function_call"
		calls = calls[:1]
	}
	body := openai.NewCompletion(openai.CompletionID(final.SessionID), created, used, finish, content, calls, usageOf(final))
	if req.Legacy {
		c := calls[0]
		body.Choices[0].Message.FunctionCall = &openai.FunctionWire{Name: c.Name, Arguments: c.Arguments}
		body.Choices[0].Message.ToolCalls = nil
	}
	writeJSON(w, http.StatusOK, body)
	rec.status = http.StatusOK
}

// fail writes a mapped HTTP error and stores the status on the access record.
// session and ignored are added when the response still has a chance to set headers.
func (s *Server) fail(w http.ResponseWriter, rec *logRec, err error, session string, ignored []string) {
	setRunHeaders(w.Header(), session, ignored)
	status := writeRunError(w, err)
	if status == 0 {
		status = http.StatusBadGateway
	}
	rec.status = status
}

// sseError writes one error event and does not write [DONE].
// openai-go treats a data payload with an error field as a stream failure.
func (s *Server) sseError(sw *sse, err error) {
	msg := err.Error()
	typ := "api_error"
	code := grok.CodeFailed
	var env *prompt.EnvelopeError
	if errors.As(err, &env) {
		code = env.APICode
		msg = env.Msg
	}
	if ge, ok := grok.AsError(err); ok {
		code = ge.Code
		msg = ge.Message
		_, typ = grokStatus(ge.Code)
	}
	_ = sw.Send(errorBody{Error: apiError{Message: msg, Type: typ, Code: code}})
}

// noteUsage copies token counts onto the access line.
func (s *Server) noteUsage(rec *logRec, final grok.Final) {
	rec.inTok = final.Usage.PromptTokens()
	rec.outTok = final.Usage.CompletionTokens()
}

// usedModel prefers the model grok reported, then the -m we passed, then the probed default.
func (s *Server) usedModel(reported, passed string) string {
	if reported != "" && reported != "unknown" {
		return reported
	}
	if passed != "" {
		return passed
	}
	return s.DefaultModel
}

// usageOf maps grok usage into the OpenAI usage object.
func usageOf(final grok.Final) openai.Usage {
	return openai.FromGrokUsage(final.Usage.PromptTokens(), final.Usage.CompletionTokens(), final.Usage.Reasoning, final.Usage.ReasoningSet)
}

// setRunHeaders sets the session and ignored-parameter headers.
// An empty session or an empty ignored list omits that header.
func setRunHeaders(h http.Header, session string, ignored []string) {
	if session != "" {
		h.Set("X-Agent-Mock-Session", session)
	}
	if v := ignoredHeader(ignored); v != "" {
		h.Set("X-Agent-Mock-Ignored", v)
	}
}

// resolveModel applies grok-known, then -model-map, then -default-model.
// The returned string is the -m value. Empty means omit -m.
func resolveModel(request string, known []string, aliases map[string]string, def string) string {
	for _, id := range known {
		if request == id {
			return request
		}
	}
	if m, ok := aliases[request]; ok {
		return m
	}
	return def
}

// firstNonEmpty returns the first non-empty string.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
