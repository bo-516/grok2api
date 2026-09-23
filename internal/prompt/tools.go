package prompt

import (
	"crypto/rand"
	"encoding/json"
	"strings"

	"github.com/shaoboli/agent-mock/internal/openai"
)

// envelope is the JSON schema and the manual that forces a reply or tool calls.
type envelope struct {
	schema       []byte
	instructions string
	names        []string
	force        bool
	maxCalls     int
	minCalls     int
}

// Outcome is a parsed envelope: either assistant text or tool calls.
type Outcome struct {
	// Reply is true when action is reply. Content is the assistant text.
	Reply bool
	// Content is the reply text. Objects are compact JSON.
	Content string
	// Calls are the function calls when Reply is false.
	Calls []openai.ToolResult
}

// buildEnvelope creates the strict anyOf schema grok 1.0.41 accepted.
// tool_choice required or a named function removes the reply action.
// parallel_tool_calls false, and a legacy functions request, set maxItems to 1.
// A named function with parallel calls allowed does not cap the count.
// When response_format is also set, the reply content property is that schema
// instead of a string; the server stringifies the object into message content.
func buildEnvelope(req *openai.Request) (*envelope, error) {
	names := make([]string, 0, len(req.Tools))
	var items []any
	allowed := req.Tools
	if req.ToolChoice.Kind == "function" {
		allowed = nil
		for _, t := range req.Tools {
			if t.Name == req.ToolChoice.Name {
				allowed = []openai.ToolDef{t}
			}
		}
	}
	for _, t := range allowed {
		names = append(names, t.Name)
		params := t.Parameters
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object"}`)
		}
		var paramsVal any
		if json.Unmarshal(params, &paramsVal) != nil {
			paramsVal = map[string]any{"type": "object"}
		}
		items = append(items, map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []string{"name", "arguments"},
			"properties": map[string]any{
				"name":      map[string]any{"enum": []string{t.Name}},
				"arguments": paramsVal,
			},
		})
	}
	actions := []string{"reply", "call_tools"}
	force := req.ToolChoice.Kind == "required" || req.ToolChoice.Kind == "function"
	if force {
		actions = []string{"call_tools"}
	}
	contentIsText := true
	var content any = map[string]any{"type": "string"}
	if req.ResponseFormat != nil && len(req.ResponseFormat.Schema) > 0 {
		var schema any
		if json.Unmarshal(req.ResponseFormat.Schema, &schema) == nil {
			content = schema
			contentIsText = false
		}
	}
	toolCalls := map[string]any{
		"type":  "array",
		"items": map[string]any{"anyOf": items},
	}
	maxCalls := 0
	minCalls := 0
	if force {
		minCalls = 1
		toolCalls["minItems"] = 1
	}
	// OpenAI allows several calls of one named function unless parallel_tool_calls is false.
	// Legacy function_call can carry only one call, so that path stays capped.
	if req.Legacy || (req.Parallel != nil && !*req.Parallel) {
		maxCalls = 1
		toolCalls["maxItems"] = 1
	}
	schema := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"action"},
		"properties": map[string]any{
			"action":     map[string]any{"enum": actions},
			"content":    content,
			"tool_calls": toolCalls,
		},
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	return &envelope{
		schema:       raw,
		instructions: instructions(allowed, force, req.ToolChoice.Name, maxCalls, contentIsText),
		names:        names,
		force:        force,
		maxCalls:     maxCalls,
		minCalls:     minCalls,
	}, nil
}

// instructions is the function manual appended to the system text.
// It tells the model to copy argument strings verbatim so a city written as 北京 stays 北京.
// maxCalls 1 asks for a single call. contentIsText allows a short preamble beside tool calls.
func instructions(tools []openai.ToolDef, force bool, only string, maxCalls int, contentIsText bool) string {
	var b strings.Builder
	b.WriteString("Functions you may call:\n")
	for _, t := range tools {
		b.WriteString("- ")
		b.WriteString(t.Name)
		if t.Description != "" {
			b.WriteString(": ")
			b.WriteString(t.Description)
		}
		b.WriteString("\n  parameters: ")
		if len(t.Parameters) == 0 {
			b.WriteString(`{"type":"object"}`)
		} else {
			b.Write(t.Parameters)
		}
		b.WriteByte('\n')
	}
	b.WriteString("\nCopy argument values verbatim from the user message. Do not translate, transliterate, or rename places or other strings.\n")
	b.WriteString("Respond only as JSON matching the schema. ")
	if force && only != "" && maxCalls == 1 {
		b.WriteString("You must call " + only + " exactly once. action must be call_tools.")
	} else if force && only != "" {
		b.WriteString("You must call " + only + ". You may call it more than once. action must be call_tools.")
	} else if force {
		b.WriteString("You must call at least one function. action must be call_tools.")
	} else {
		b.WriteString(`Use action "reply" and put the user-facing text in content when no function is needed. Use action "call_tools" with tool_calls when a function is needed.`)
	}
	if contentIsText {
		b.WriteString(" When action is call_tools, content may be a short preamble or an empty string.")
	}
	return b.String()
}

// toolEnvelope is the JSON object the model returns for a tool turn.
type toolEnvelope struct {
	Action    string          `json:"action"`
	Content   json.RawMessage `json:"content"`
	ToolCalls []struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"tool_calls"`
}

// ParseOutcome reads structured_output or the result text as an envelope.
// Unknown tool names are returned to the caller. Arguments are kept as a string
// even when they are not a JSON object, matching OpenAI. A broken envelope, or
// call_tools with fewer calls than minCalls, becomes a normal text reply.
// A strict tool whose arguments do not match its schema is grok_structured_output_failed.
// tools carry strict schemas. minCalls and maxCalls come from Rendered. maxCalls 0 means no cap.
func ParseOutcome(structured []byte, text string, tools []openai.ToolDef, minCalls, maxCalls int) (*Outcome, error) {
	raw := structured
	if len(strings.TrimSpace(string(raw))) == 0 {
		raw = []byte(strings.TrimSpace(text))
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, envelopeErr("grok_structured_output_failed", "grok returned an empty tool envelope")
	}
	env, ok := decodeEnvelope(raw)
	if !ok && len(strings.TrimSpace(string(structured))) == 0 {
		var inner string
		if json.Unmarshal(raw, &inner) == nil && strings.TrimSpace(inner) != "" {
			return ParseOutcome([]byte(inner), "", tools, minCalls, maxCalls)
		}
	}
	if !ok {
		return textReply(text, raw), nil
	}
	content, err := stringifyContent(env.Content)
	if err != nil {
		return textReply(text, raw), nil
	}
	if env.Action == "reply" {
		return &Outcome{Reply: true, Content: content}, nil
	}
	calls, err := collectCalls(env, tools)
	if err != nil {
		return nil, err
	}
	if maxCalls > 0 && len(calls) > maxCalls {
		calls = calls[:maxCalls]
	}
	if len(calls) == 0 || len(calls) < minCalls {
		return &Outcome{Reply: true, Content: content}, nil
	}
	return &Outcome{Content: content, Calls: calls}, nil
}

// decodeEnvelope parses one envelope object. ok is false when the JSON is not an envelope.
// A JSON string or a missing action is not an envelope. The caller then falls back to text.
func decodeEnvelope(raw []byte) (toolEnvelope, bool) {
	var env toolEnvelope
	if json.Unmarshal(raw, &env) != nil {
		return toolEnvelope{}, false
	}
	if env.Action != "reply" && env.Action != "call_tools" {
		return toolEnvelope{}, false
	}
	return env, true
}

// textReply is the 200 fallback when the model did not produce a usable envelope.
// Non-empty text wins. Otherwise the raw bytes are the assistant content.
func textReply(text string, raw []byte) *Outcome {
	if s := strings.TrimSpace(text); s != "" {
		return &Outcome{Reply: true, Content: s}
	}
	return &Outcome{Reply: true, Content: strings.TrimSpace(string(raw))}
}

// collectCalls turns envelope tool_calls into OpenAI tool results.
// An empty name is skipped. Strict tools are checked against their schema.
// A strict mismatch returns grok_structured_output_failed and no partial list.
func collectCalls(env toolEnvelope, tools []openai.ToolDef) ([]openai.ToolResult, error) {
	var out []openai.ToolResult
	for _, c := range env.ToolCalls {
		if c.Name == "" {
			continue
		}
		args, obj, isObj := argumentValue(c.Arguments)
		if def, ok := findTool(tools, c.Name); ok && def.Strict {
			if !isObj {
				return nil, envelopeErr("grok_structured_output_failed", "tool "+c.Name+" arguments did not match the strict schema")
			}
			if err := openai.MatchStrictArgs(def.Parameters, obj); err != nil {
				return nil, envelopeErr("grok_structured_output_failed", "tool "+c.Name+" arguments did not match the strict schema")
			}
		}
		id, err := newCallID()
		if err != nil {
			return nil, err
		}
		out = append(out, openai.ToolResult{ID: id, Name: c.Name, Arguments: args})
	}
	return out, nil
}

// argumentValue converts envelope arguments into the OpenAI arguments string.
// An object is compact JSON. A JSON string is unquoted. Any other JSON value is compact JSON.
// Invalid JSON is returned as the raw text. isObj is true only for a JSON object, and obj is that object.
func argumentValue(raw json.RawMessage) (args string, obj json.RawMessage, isObj bool) {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || string(raw) == "null" {
		return "{}", nil, false
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw), nil, false
	}
	if _, ok := v.(map[string]any); ok {
		s, err := compact(raw)
		if err != nil {
			return string(raw), nil, false
		}
		return s, json.RawMessage(s), true
	}
	if s, ok := v.(string); ok {
		return s, nil, false
	}
	s, err := compact(raw)
	if err != nil {
		return string(raw), nil, false
	}
	return s, nil, false
}

// findTool returns the declared tool with this name. The second result is false when name is unknown.
func findTool(tools []openai.ToolDef, name string) (openai.ToolDef, bool) {
	for _, t := range tools {
		if t.Name == name {
			return t, true
		}
	}
	return openai.ToolDef{}, false
}

// stringifyContent turns envelope content into the assistant string.
// A JSON string is unquoted. An object or array is compact JSON. Null is empty.
func stringifyContent(raw json.RawMessage) (string, error) {
	raw = []byte(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", err
		}
		return s, nil
	}
	return compact(raw)
}

// compact re-encodes JSON with no extra space.
func compact(raw json.RawMessage) (string, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// newCallID returns call_ plus 24 letters and digits.
// A rand failure is returned to the caller, which fails the request rather than
// emitting an id that does not match the documented pattern.
func newCallID() (string, error) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	buf := make([]byte, 24)
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	for i, v := range raw {
		buf[i] = alphabet[int(v)%len(alphabet)]
	}
	return "call_" + string(buf), nil
}

// EnvelopeError is a tool-envelope failure the HTTP layer maps to 502.
// APICode is grok_bad_output or grok_structured_output_failed.
type EnvelopeError struct {
	// APICode is the OpenAI error.code.
	APICode string
	// Msg is the client-facing message.
	Msg string
}

// Error returns Msg.
func (e *EnvelopeError) Error() string { return e.Msg }

// envelopeErr builds an EnvelopeError. code selects bad output versus a broken envelope.
func envelopeErr(code, msg string) error {
	return &EnvelopeError{APICode: code, Msg: msg}
}
