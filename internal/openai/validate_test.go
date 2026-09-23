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
		{`{"model":"m","messages":[{"role":"function","content":"x"}]}`, "unsupported_role", "function"},
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
