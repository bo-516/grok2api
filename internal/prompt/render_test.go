package prompt

import (
	"strings"
	"testing"

	"github.com/shaoboli/agent-mock/internal/openai"
)

// TestRenderSingleUser keeps one user message raw and puts system text after the preamble.
func TestRenderSingleUser(t *testing.T) {
	out, err := Render(&openai.Request{Messages: []openai.Message{
		{Role: "system", Text: "Answer in one word."},
		{Role: "user", Text: "Capital of France?"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Prompt != "Capital of France?" {
		t.Fatal(out.Prompt)
	}
	if !strings.HasPrefix(out.SystemOverride, Preamble) || !strings.Contains(out.SystemOverride, "# Instructions from the API caller") || !strings.Contains(out.SystemOverride, "Answer in one word.") {
		t.Fatal(out.SystemOverride)
	}
	if out.SystemInFile {
		t.Fatal("unexpected spill")
	}
}

// TestRenderTranscript preserves tool call ids and tool results.
func TestRenderTranscript(t *testing.T) {
	out, err := Render(&openai.Request{Messages: []openai.Message{
		{Role: "user", Text: "北京今天天气怎么样？"},
		{Role: "assistant", ToolCalls: []openai.ToolCall{{ID: "call_abc", Name: "get_weather", Arguments: `{"city":"北京"}`}}},
		{Role: "tool", ToolCallID: "call_abc", Text: `{"temp_c":20}`},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{`<message role="user">北京今天天气怎么样？</message>`, `id="call_abc"`, `name="get_weather"`, `{"city":"北京"}`, `tool_call_id="call_abc"`, `{"temp_c":20}`, "Write the assistant's next message."} {
		if !strings.Contains(out.Prompt, s) {
			t.Fatalf("missing %s in %s", s, out.Prompt)
		}
	}
}

// TestRenderMovesLargeSystem puts an oversized override at the top of the prompt file.
func TestRenderMovesLargeSystem(t *testing.T) {
	big := strings.Repeat("a", maxInline)
	out, err := Render(&openai.Request{Messages: []openai.Message{
		{Role: "system", Text: big},
		{Role: "user", Text: "ping"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !out.SystemInFile || out.SystemOverride != "" || !strings.HasPrefix(out.Prompt, "<system>\n") || !strings.Contains(out.Prompt, "ping") {
		t.Fatalf("inFile %v override %d prompt %s", out.SystemInFile, len(out.SystemOverride), out.Prompt[:40])
	}
}

// TestEnvelopeAnyOf builds the strict schema and forces a named function.
func TestEnvelopeAnyOf(t *testing.T) {
	req := &openai.Request{
		Messages:   []openai.Message{{Role: "user", Text: "北京今天天气怎么样？"}},
		Tools:      []openai.ToolDef{{Name: "get_weather", Description: "weather", Parameters: []byte(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`)}},
		ToolChoice: openai.ToolChoice{Kind: "function", Name: "get_weather"},
	}
	out, err := Render(req)
	if err != nil {
		t.Fatal(err)
	}
	schema := string(out.JSONSchema)
	if !strings.Contains(schema, "anyOf") || !strings.Contains(schema, "get_weather") || !strings.Contains(schema, `"minItems":1`) || !strings.Contains(schema, `"maxItems":1`) {
		t.Fatal(schema)
	}
	if !strings.Contains(out.SystemOverride, "get_weather") || !strings.Contains(out.SystemOverride, "verbatim") {
		t.Fatal(out.SystemOverride)
	}
	if !out.ForceCalls || out.MaxCalls != 1 || out.MinCalls != 1 {
		t.Fatalf("%+v", out)
	}
}

// TestToolChoiceNoneSkipsEnvelope.
func TestToolChoiceNoneSkipsEnvelope(t *testing.T) {
	out, err := Render(&openai.Request{
		Messages:   []openai.Message{{Role: "user", Text: "hi"}},
		Tools:      []openai.ToolDef{{Name: "get_weather"}},
		ToolChoice: openai.ToolChoice{Kind: "none"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Envelope || len(out.JSONSchema) != 0 {
		t.Fatalf("%+v %s", out.Envelope, out.JSONSchema)
	}
}

// TestParseOutcomeReadsRecordedEnvelope uses the tools fixture's structured object shape.
func TestParseOutcomeReadsRecordedEnvelope(t *testing.T) {
	raw := []byte(`{"action":"call_tools","tool_calls":[{"name":"get_weather","arguments":{"city":"北京"}}]}`)
	out, err := ParseOutcome(raw, "", []string{"get_weather"}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if out.Reply || len(out.Calls) != 1 || out.Calls[0].Name != "get_weather" || !strings.Contains(out.Calls[0].Arguments, "北京") {
		t.Fatalf("%+v", out)
	}
	if !strings.HasPrefix(out.Calls[0].ID, "call_") || len(out.Calls[0].ID) != len("call_")+24 {
		t.Fatal(out.Calls[0].ID)
	}
	if _, err := ParseOutcome([]byte(`{"action":"call_tools","tool_calls":[{"name":"rm","arguments":{"x":1}}]}`), "", []string{"get_weather"}, 0, 0); err == nil {
		t.Fatal("expected unknown tool")
	}
	if _, err := ParseOutcome([]byte(`{"action":"call_tools","tool_calls":[{"name":"get_weather","arguments":"nope"}]}`), "", []string{"get_weather"}, 0, 0); err == nil {
		t.Fatal("expected bad arguments")
	}
}
