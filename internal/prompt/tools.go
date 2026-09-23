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
// parallel_tool_calls false and a named function set maxItems to 1.
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
	var content any = map[string]any{"type": "string"}
	if req.ResponseFormat != nil && len(req.ResponseFormat.Schema) > 0 {
		var schema any
		if json.Unmarshal(req.ResponseFormat.Schema, &schema) == nil {
			content = schema
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
	if (req.Parallel != nil && !*req.Parallel) || req.ToolChoice.Kind == "function" {
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
		instructions: instructions(allowed, force, req.ToolChoice.Name),
		names:        names,
		force:        force,
		maxCalls:     maxCalls,
		minCalls:     minCalls,
	}, nil
}

// instructions is the function manual appended to the system text.
// It tells the model to copy argument strings verbatim so a city written as 北京 stays 北京.
func instructions(tools []openai.ToolDef, force bool, only string) string {
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
	if force && only != "" {
		b.WriteString("You must call " + only + " exactly once. action must be call_tools.")
	} else if force {
		b.WriteString("You must call at least one function. action must be call_tools.")
	} else {
		b.WriteString(`Use action "reply" and put the user-facing text in content when no function is needed. Use action "call_tools" with tool_calls when a function is needed.`)
	}
	return b.String()
}

// ParseOutcome reads structured_output or the result text as an envelope.
// Unknown tool names and non-object arguments are grok_bad_output (code returned
// as an error string prefix the server maps). Invalid JSON is grok_structured_output_failed.
// limits come from Rendered. names is the allow list.
func ParseOutcome(structured []byte, text string, names []string, minCalls, maxCalls int) (*Outcome, error) {
	raw := structured
	if len(raw) == 0 {
		raw = []byte(strings.TrimSpace(text))
	}
	if len(raw) == 0 {
		return nil, envelopeErr("grok_structured_output_failed", "grok returned an empty tool envelope")
	}
	var env struct {
		Action    string          `json:"action"`
		Content   json.RawMessage `json:"content"`
		ToolCalls []struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"tool_calls"`
	}
	if json.Unmarshal(raw, &env) != nil || (env.Action != "reply" && env.Action != "call_tools") {
		// The text may be a JSON string wrapping the object. Try once more if structured was empty.
		if len(structured) == 0 {
			var inner string
			if json.Unmarshal(raw, &inner) == nil {
				return ParseOutcome([]byte(inner), "", names, minCalls, maxCalls)
			}
		}
		return nil, envelopeErr("grok_structured_output_failed", "grok tool envelope was not valid JSON with action reply or call_tools")
	}
	if env.Action == "reply" {
		content, err := stringifyContent(env.Content)
		if err != nil {
			return nil, envelopeErr("grok_structured_output_failed", "grok reply content was not usable JSON")
		}
		return &Outcome{Reply: true, Content: content}, nil
	}
	if len(env.ToolCalls) < minCalls || (maxCalls > 0 && len(env.ToolCalls) > maxCalls) {
		return nil, envelopeErr("grok_bad_output", "grok tool envelope had the wrong number of tool calls")
	}
	allow := map[string]bool{}
	for _, n := range names {
		allow[n] = true
	}
	out := &Outcome{}
	for _, c := range env.ToolCalls {
		if !allow[c.Name] {
			return nil, envelopeErr("grok_bad_output", "grok called unknown tool "+c.Name)
		}
		if !jsonObject(c.Arguments) {
			return nil, envelopeErr("grok_bad_output", "grok tool arguments for "+c.Name+" were not a JSON object")
		}
		args, err := compact(c.Arguments)
		if err != nil {
			return nil, envelopeErr("grok_bad_output", "grok tool arguments for "+c.Name+" were not valid JSON")
		}
		id, err := newCallID()
		if err != nil {
			return nil, err
		}
		out.Calls = append(out.Calls, openai.ToolResult{ID: id, Name: c.Name, Arguments: args})
	}
	return out, nil
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

// jsonObject reports whether raw is a JSON object.
func jsonObject(raw json.RawMessage) bool {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return false
	}
	_, ok := v.(map[string]any)
	return ok
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
