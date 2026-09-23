package openai

import (
	"bytes"
	"encoding/json"
)

// fillLegacy maps the deprecated functions/function_call pair onto tools/tool_choice.
// Using both pairs in one request is 400. function_call without functions is 400.
// A functions request sets Request.Legacy so the response uses function_call.
// Omitted function_call means auto, matching the old API default.
func fillLegacy(raw map[string]json.RawMessage, req *Request) error {
	_, hasFn := nonNull(raw["functions"])
	_, hasCall := nonNull(raw["function_call"])
	_, hasTools := nonNull(raw["tools"])
	_, hasChoice := nonNull(raw["tool_choice"])
	if hasFn && hasTools {
		return bad("unsupported_parameter", `parameters "tools" and "functions" cannot be used together`)
	}
	if hasCall && hasChoice {
		return bad("unsupported_parameter", `parameters "tool_choice" and "function_call" cannot be used together`)
	}
	if hasFn && hasChoice {
		return bad("unsupported_parameter", `parameter "functions" cannot be combined with "tool_choice"`)
	}
	if hasCall && !hasFn && !hasTools {
		return bad("unsupported_parameter", `parameter "function_call" requires "functions"`)
	}
	if !hasFn {
		return nil
	}
	arrRaw := raw["functions"]
	var arr []map[string]json.RawMessage
	if json.Unmarshal(arrRaw, &arr) != nil {
		return bad("unsupported_parameter", `parameter "functions" must be an array`)
	}
	for _, fn := range arr {
		name := unquote(fn["name"])
		if name == "" {
			return bad("unsupported_parameter", `parameter "functions" is missing a function name`)
		}
		def, err := toolDef(name, unquote(fn["description"]), fn["parameters"], fn["strict"])
		if err != nil {
			return err
		}
		req.Tools = append(req.Tools, def)
	}
	req.Legacy = true
	req.ToolChoice = ToolChoice{Kind: "auto"}
	if hasCall {
		choice, err := parseFunctionChoice(raw["function_call"], req.Tools)
		if err != nil {
			return err
		}
		req.ToolChoice = choice
	}
	return nil
}

// toolDef builds one function definition and checks a strict schema.
// strictRaw may be missing. A non-boolean strict is 400. A strict schema that
// does not set additionalProperties false, or that leaves a property out of required, is 400.
func toolDef(name, description string, params, strictRaw json.RawMessage) (ToolDef, error) {
	def := ToolDef{Name: name, Description: description}
	if p, ok := nonNull(params); ok {
		def.Parameters = append(json.RawMessage(nil), p...)
	}
	if s, ok := nonNull(strictRaw); ok {
		var b bool
		if json.Unmarshal(s, &b) != nil {
			return ToolDef{}, bad("unsupported_parameter", `parameter "strict" must be a boolean`)
		}
		def.Strict = b
	}
	if def.Strict {
		if err := CheckStrictSchema(def.Name, def.Parameters); err != nil {
			return ToolDef{}, err
		}
	}
	return def, nil
}

// parseFunctionChoice reads function_call "auto", "none", or {"name":"..."}.
// An unknown name is 400. Any other shape is 400.
func parseFunctionChoice(raw json.RawMessage, tools []ToolDef) (ToolChoice, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return ToolChoice{}, bad("unsupported_parameter", `parameter "function_call" is invalid`)
		}
		switch s {
		case "auto", "none":
			return ToolChoice{Kind: s}, nil
		default:
			return ToolChoice{}, bad("unsupported_parameter", `parameter "function_call" value "`+s+`" is unsupported`)
		}
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return ToolChoice{}, bad("unsupported_parameter", `parameter "function_call" must be a string or object`)
	}
	name := unquote(obj["name"])
	if name == "" {
		return ToolChoice{}, bad("unsupported_parameter", `parameter "function_call" is missing a function name`)
	}
	if !hasTool(tools, name) {
		return ToolChoice{}, bad("unsupported_parameter", `parameter "function_call" names unknown function "`+name+`"`)
	}
	return ToolChoice{Kind: "function", Name: name}, nil
}

// parseFunctionCall reads an assistant function_call object from message history.
// arguments may be a JSON string or a JSON value; both become the arguments string.
// A missing name is 400.
func parseFunctionCall(raw json.RawMessage) (ToolCall, error) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return ToolCall{}, bad("unsupported_parameter", `parameter "function_call" must be an object`)
	}
	name := unquote(obj["name"])
	if name == "" {
		return ToolCall{}, bad("unsupported_parameter", `function_call name is required`)
	}
	return ToolCall{Name: name, Arguments: asArgumentString(obj["arguments"])}, nil
}

// asArgumentString accepts the OpenAI arguments string or a raw JSON value.
// A JSON string is unquoted. An object or array is compacted. Invalid JSON is kept as text.
// Empty or null becomes an empty string so a missing field does not invent {}.
func asArgumentString(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
	}
	var v any
	if json.Unmarshal(raw, &v) == nil {
		if b, err := json.Marshal(v); err == nil {
			return string(b)
		}
	}
	return string(raw)
}

// nonNull reports whether raw is present and not JSON null.
// The returned slice is trimmed. A missing field is false.
func nonNull(raw json.RawMessage) (json.RawMessage, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false
	}
	return raw, true
}
