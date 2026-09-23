package openai

import (
	"bytes"
	"encoding/json"
	"strings"
)

// ignoredFields is the sampling parameters agent-mock accepts and does not apply.
// The order is the order of the X-Agent-Mock-Ignored header.
var ignoredFields = []string{
	"temperature", "top_p", "max_tokens", "max_completion_tokens",
	"stop", "seed", "presence_penalty", "frequency_penalty",
}

// allowedEfforts are the reasoning_effort values grok 1.0.41 documents,
// plus the per-model id "deep". "turbo" is rejected on purpose.
var allowedEfforts = map[string]bool{
	"none": true, "minimal": true, "low": true, "medium": true,
	"high": true, "xhigh": true, "max": true, "deep": true,
}

// Parse decodes a Chat Completions body and rejects anything this server cannot do.
// Malformed JSON is invalid_json. n>1, logprobs, unknown roles, and non-text parts
// are 400 and the message names the field. A nil error means req is safe to render.
func Parse(body []byte) (*Request, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, bad("invalid_json", "request body is empty")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, bad("invalid_json", "request body is not valid JSON")
	}
	req := &Request{ToolChoice: ToolChoice{Kind: "auto"}}
	if err := rejectUnsupported(raw); err != nil {
		return nil, err
	}
	req.Ignored = presentIgnored(raw)
	if m, ok := raw["model"]; ok {
		if err := json.Unmarshal(m, &req.Model); err != nil {
			return nil, bad("unsupported_parameter", `parameter "model" must be a string`)
		}
	}
	if err := fillMessages(raw["messages"], req); err != nil {
		return nil, err
	}
	if len(req.Messages) == 0 {
		return nil, bad("unsupported_parameter", `parameter "messages" must contain at least one message`)
	}
	if s, ok := raw["stream"]; ok {
		if err := json.Unmarshal(s, &req.Stream); err != nil {
			return nil, bad("unsupported_parameter", `parameter "stream" must be a boolean`)
		}
	}
	if opt, ok := raw["stream_options"]; ok && string(bytes.TrimSpace(opt)) != "null" {
		var opts map[string]json.RawMessage
		if json.Unmarshal(opt, &opts) != nil {
			return nil, bad("unsupported_parameter", `parameter "stream_options" must be an object`)
		}
		if u, ok := opts["include_usage"]; ok {
			_ = json.Unmarshal(u, &req.IncludeUsage)
		}
	}
	if err := fillTools(raw, req); err != nil {
		return nil, err
	}
	if err := fillLegacy(raw, req); err != nil {
		return nil, err
	}
	if err := fillFormat(raw["response_format"], req); err != nil {
		return nil, err
	}
	if effort, ok := raw["reasoning_effort"]; ok && string(bytes.TrimSpace(effort)) != "null" {
		var s string
		if json.Unmarshal(effort, &s) != nil {
			return nil, bad("unsupported_parameter", `parameter "reasoning_effort" must be a string`)
		}
		if !allowedEfforts[s] {
			return nil, bad("unsupported_parameter", `parameter "reasoning_effort" must be one of none, minimal, low, medium, high, xhigh, max`)
		}
		req.ReasoningEffort = s
	}
	return req, nil
}

// rejectUnsupported returns 400 for n>1 and logprobs. Other unknown fields are ignored
// so SDK metadata does not break local dev. The message names the offending field.
func rejectUnsupported(raw map[string]json.RawMessage) error {
	if n, ok := raw["n"]; ok && string(bytes.TrimSpace(n)) != "null" {
		var v int
		if json.Unmarshal(n, &v) != nil {
			return bad("unsupported_parameter", `parameter "n" must be 1`)
		}
		if v != 1 {
			return bad("unsupported_parameter", `parameter "n" is unsupported when it is not 1`)
		}
	}
	if lp, ok := raw["logprobs"]; ok && string(bytes.TrimSpace(lp)) != "null" {
		var v bool
		if json.Unmarshal(lp, &v) != nil || v {
			return bad("unsupported_parameter", `parameter "logprobs" is unsupported`)
		}
	}
	if tl, ok := raw["top_logprobs"]; ok && string(bytes.TrimSpace(tl)) != "null" {
		return bad("unsupported_parameter", `parameter "top_logprobs" is unsupported`)
	}
	return nil
}

// presentIgnored lists inert sampling fields that the client actually sent.
func presentIgnored(raw map[string]json.RawMessage) []string {
	var out []string
	for _, name := range ignoredFields {
		v, ok := raw[name]
		if !ok || string(bytes.TrimSpace(v)) == "null" {
			continue
		}
		out = append(out, name)
	}
	return out
}

// fillMessages decodes messages and rejects unknown roles and non-text parts.
func fillMessages(raw json.RawMessage, req *Request) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return bad("unsupported_parameter", `parameter "messages" is required`)
	}
	var arr []map[string]json.RawMessage
	if json.Unmarshal(raw, &arr) != nil {
		return bad("unsupported_parameter", `parameter "messages" must be an array`)
	}
	for _, m := range arr {
		role := unquote(m["role"])
		switch role {
		case "system", "developer", "user", "assistant", "tool", "function":
		default:
			if role == "" {
				role = "missing"
			}
			return bad("unsupported_role", `role "`+role+`" is unsupported`)
		}
		text, err := contentText(m["content"])
		if err != nil {
			return err
		}
		msg := Message{Role: role, Text: text, ToolCallID: unquote(m["tool_call_id"]), Name: unquote(m["name"])}
		if role == "function" && msg.Name == "" {
			return bad("unsupported_parameter", `role "function" requires name`)
		}
		if calls, ok := m["tool_calls"]; ok && string(bytes.TrimSpace(calls)) != "null" {
			parsed, err := parseToolCalls(calls)
			if err != nil {
				return err
			}
			msg.ToolCalls = parsed
		}
		if fc, ok := m["function_call"]; ok && string(bytes.TrimSpace(fc)) != "null" {
			call, err := parseFunctionCall(fc)
			if err != nil {
				return err
			}
			msg.ToolCalls = append(msg.ToolCalls, call)
		}
		req.Messages = append(req.Messages, msg)
	}
	return nil
}

// contentText accepts a string or an array of text parts joined by newlines.
// image_url, input_audio, file, and any other part type return unsupported_content_part.
func contentText(raw json.RawMessage) (string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return "", bad("invalid_json", "message content is not valid JSON")
		}
		return s, nil
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return "", bad("unsupported_content_part", `content must be a string or an array of text parts`)
	}
	var b strings.Builder
	for i, p := range parts {
		typ := unquote(p["type"])
		if typ == "" {
			if _, ok := p["text"]; ok {
				typ = "text"
			}
		}
		if typ != "text" {
			if typ == "" {
				typ = "unknown"
			}
			return "", bad("unsupported_content_part", `content part "`+typ+`" is unsupported`)
		}
		piece := unquote(p["text"])
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(piece)
	}
	return b.String(), nil
}

// parseToolCalls reads assistant tool_calls. A missing function name is 400.
func parseToolCalls(raw json.RawMessage) ([]ToolCall, error) {
	var arr []map[string]json.RawMessage
	if json.Unmarshal(raw, &arr) != nil {
		return nil, bad("unsupported_parameter", `parameter "tool_calls" must be an array`)
	}
	out := make([]ToolCall, 0, len(arr))
	for _, c := range arr {
		fn := map[string]json.RawMessage{}
		if f, ok := c["function"]; ok {
			_ = json.Unmarshal(f, &fn)
		}
		name := unquote(fn["name"])
		if name == "" {
			return nil, bad("unsupported_parameter", `tool_calls function name is required`)
		}
		out = append(out, ToolCall{ID: unquote(c["id"]), Name: name, Arguments: asArgumentString(fn["arguments"])})
	}
	return out, nil
}

// fillTools parses tools, tool_choice, and parallel_tool_calls.
// With no tools, tool_choice is ignored. With tools and no tool_choice, kind is auto.
func fillTools(raw map[string]json.RawMessage, req *Request) error {
	if t, ok := raw["tools"]; ok && string(bytes.TrimSpace(t)) != "null" {
		var arr []map[string]json.RawMessage
		if json.Unmarshal(t, &arr) != nil {
			return bad("unsupported_parameter", `parameter "tools" must be an array`)
		}
		for _, tool := range arr {
			typ := unquote(tool["type"])
			if typ != "" && typ != "function" {
				return bad("unsupported_parameter", `tool type "`+typ+`" is unsupported`)
			}
			fn := map[string]json.RawMessage{}
			if f, ok := tool["function"]; ok {
				if json.Unmarshal(f, &fn) != nil {
					return bad("unsupported_parameter", `parameter "tools" function must be an object`)
				}
			}
			name := unquote(fn["name"])
			if name == "" {
				return bad("unsupported_parameter", `parameter "tools" is missing a function name`)
			}
			def, err := toolDef(name, unquote(fn["description"]), fn["parameters"], fn["strict"])
			if err != nil {
				return err
			}
			req.Tools = append(req.Tools, def)
		}
	}
	if len(req.Tools) == 0 {
		req.ToolChoice = ToolChoice{}
	}
	if c, ok := raw["tool_choice"]; ok && string(bytes.TrimSpace(c)) != "null" && len(req.Tools) > 0 {
		choice, err := parseToolChoice(c, req.Tools)
		if err != nil {
			return err
		}
		req.ToolChoice = choice
	}
	if p, ok := raw["parallel_tool_calls"]; ok && string(bytes.TrimSpace(p)) != "null" {
		var v bool
		if json.Unmarshal(p, &v) != nil {
			return bad("unsupported_parameter", `parameter "parallel_tool_calls" must be a boolean`)
		}
		req.Parallel = &v
	}
	return nil
}

// parseToolChoice accepts auto, none, required, or {"type":"function","function":{"name":...}}.
func parseToolChoice(raw json.RawMessage, tools []ToolDef) (ToolChoice, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return ToolChoice{}, bad("unsupported_parameter", `parameter "tool_choice" is invalid`)
		}
		switch s {
		case "auto", "none", "required":
			return ToolChoice{Kind: s}, nil
		default:
			return ToolChoice{}, bad("unsupported_parameter", `parameter "tool_choice" value "`+s+`" is unsupported`)
		}
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return ToolChoice{}, bad("unsupported_parameter", `parameter "tool_choice" must be a string or object`)
	}
	var fn map[string]json.RawMessage
	if f, ok := obj["function"]; ok {
		_ = json.Unmarshal(f, &fn)
	}
	name := unquote(fn["name"])
	if name == "" {
		return ToolChoice{}, bad("unsupported_parameter", `parameter "tool_choice" is missing a function name`)
	}
	if !hasTool(tools, name) {
		return ToolChoice{}, bad("unsupported_parameter", `parameter "tool_choice" names unknown function "`+name+`"`)
	}
	return ToolChoice{Kind: "function", Name: name}, nil
}

// fillFormat maps response_format json_object and json_schema onto a schema.
// type text and an omitted field leave ResponseFormat nil. A schema over 96 KiB is 400.
func fillFormat(raw json.RawMessage, req *Request) error {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return nil
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return bad("unsupported_parameter", `parameter "response_format" must be an object`)
	}
	switch unquote(obj["type"]) {
	case "", "text":
		return nil
	case "json_object":
		req.ResponseFormat = &ResponseFormat{Type: "json_object", Schema: json.RawMessage(`{"type":"object"}`)}
		return nil
	case "json_schema":
		js := map[string]json.RawMessage{}
		if rawJS, ok := obj["json_schema"]; ok {
			if json.Unmarshal(rawJS, &js) != nil {
				return bad("unsupported_parameter", `parameter "response_format" json_schema must be an object`)
			}
		}
		schema, ok := js["schema"]
		if !ok || string(bytes.TrimSpace(schema)) == "null" {
			return bad("unsupported_parameter", `parameter "response_format" is missing json_schema.schema`)
		}
		if len(schema) > MaxArgBytes {
			return bad("unsupported_parameter", `parameter "response_format" schema exceeds 96 KiB`)
		}
		req.ResponseFormat = &ResponseFormat{Type: "json_schema", Name: unquote(js["name"]), Schema: append(json.RawMessage(nil), schema...)}
		return nil
	default:
		return bad("unsupported_parameter", `parameter "response_format" type "`+unquote(obj["type"])+`" is unsupported`)
	}
}

// hasTool reports whether name is one of the declared tools.
func hasTool(tools []ToolDef, name string) bool {
	for _, t := range tools {
		if t.Name == name {
			return true
		}
	}
	return false
}

// unquote decodes a JSON string. A non-string or empty value returns "".
func unquote(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}
