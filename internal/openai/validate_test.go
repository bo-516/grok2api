package openai

import (
	"strings"
	"testing"
)

// TestParseText accepts a string message and records inert sampling fields.
func TestParseText(t *testing.T) {
	req, err := Parse([]byte(`{"model":"gpt-4o-mini","temperature":0.2,"top_p":1,"stop":"\n","seed":1,"messages":[{"role":"user","content":"Hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.Model != "gpt-4o-mini" || req.Messages[0].Text != "Hi" {
		t.Fatalf("%+v", req)
	}
	joined := strings.Join(req.Ignored, ",")
	for _, name := range []string{"temperature", "top_p", "stop", "seed"} {
		if !strings.Contains(joined, name) {
			t.Fatal(joined)
		}
	}
}

// TestParseTextParts joins text parts and rejects image, audio, and file parts.
func TestParseTextParts(t *testing.T) {
	req, err := Parse([]byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}]}`))
	if err != nil || req.Messages[0].Text != "a\nb" {
		t.Fatal(err, req)
	}
	for _, part := range []string{"image_url", "input_audio", "file"} {
		_, err := Parse([]byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"` + part + `","text":"x"}]}]}`))
		re, ok := err.(*RequestError)
		if !ok || re.Code != "unsupported_content_part" || !strings.Contains(re.Message, part) {
			t.Fatal(part, err)
		}
	}
}

// TestParseRejects covers n, logprobs, roles, and reasoning_effort.
func TestParseRejects(t *testing.T) {
	cases := []struct {
		body, code, field string
	}{
		{`{"model":"m","n":2,"messages":[{"role":"user","content":"x"}]}`, "unsupported_parameter", "n"},
		{`{"model":"m","logprobs":true,"messages":[{"role":"user","content":"x"}]}`, "unsupported_parameter", "logprobs"},
		{`{"model":"m","messages":[{"role":"critic","content":"x"}]}`, "unsupported_role", "critic"},
		{`{"model":"m","reasoning_effort":"turbo","messages":[{"role":"user","content":"x"}]}`, "unsupported_parameter", "reasoning_effort"},
		{`not-json`, "invalid_json", ""},
	}
	for _, tc := range cases {
		_, err := Parse([]byte(tc.body))
		re, ok := err.(*RequestError)
		if !ok || re.Code != tc.code || (tc.field != "" && !strings.Contains(re.Message, tc.field)) {
			t.Fatalf("%s -> %v", tc.body, err)
		}
	}
	req, err := Parse([]byte(`{"model":"m","reasoning_effort":"low","messages":[{"role":"developer","content":"Be brief."},{"role":"user","content":"Hi"}]}`))
	if err != nil || req.ReasoningEffort != "low" || req.Messages[0].Role != "developer" {
		t.Fatal(err, req)
	}
}

// TestParseTools reads a function tool and a named tool_choice.
func TestParseTools(t *testing.T) {
	body := `{"model":"m","parallel_tool_calls":false,"tool_choice":{"type":"function","function":{"name":"get_weather"}},"messages":[{"role":"user","content":"北京"}],"tools":[{"type":"function","function":{"name":"get_weather","description":"weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}]}`
	req, err := Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "get_weather" || req.ToolChoice.Kind != "function" || req.ToolChoice.Name != "get_weather" {
		t.Fatalf("%+v", req)
	}
	if req.Parallel == nil || *req.Parallel {
		t.Fatal(req.Parallel)
	}
}

// TestParseLegacy maps functions and function_call onto tools, and accepts a function result.
func TestParseLegacy(t *testing.T) {
	body := `{"model":"m","function_call":{"name":"get_weather"},"functions":[{"name":"get_weather","description":"weather","parameters":{"type":"object"}}],"messages":[{"role":"user","content":"北京"},{"role":"assistant","function_call":{"name":"get_weather","arguments":"{\"city\":\"北京\"}"}},{"role":"function","name":"get_weather","content":"20"}]}`
	req, err := Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if !req.Legacy || req.ToolChoice.Kind != "function" || req.ToolChoice.Name != "get_weather" || len(req.Tools) != 1 {
		t.Fatalf("%+v", req)
	}
	if req.Messages[1].ToolCalls[0].Arguments != `{"city":"北京"}` || req.Messages[2].Name != "get_weather" || req.Messages[2].Text != "20" {
		t.Fatalf("%+v", req.Messages)
	}
	if _, err := Parse([]byte(`{"model":"m","messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{"name":"a"}}],"functions":[{"name":"b"}]}`)); err == nil {
		t.Fatal("expected tools+functions to fail")
	}
}

// TestParseStrictSchema rejects a strict function whose properties are not all required.
func TestParseStrictSchema(t *testing.T) {
	loose := `{"model":"m","messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{"name":"get_weather","strict":true,"parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]}`
	_, err := Parse([]byte(loose))
	re, ok := err.(*RequestError)
	if !ok || re.Code != "unsupported_parameter" || !strings.Contains(re.Message, "get_weather") || !strings.Contains(re.Message, "additionalProperties") {
		t.Fatal(err)
	}
	okBody := `{"model":"m","messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{"name":"get_weather","strict":true,"parameters":{"type":"object","additionalProperties":false,"required":["city"],"properties":{"city":{"type":"string"}}}}}]}`
	req, err := Parse([]byte(okBody))
	if err != nil || !req.Tools[0].Strict {
		t.Fatal(err, req)
	}
}
