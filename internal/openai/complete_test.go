package openai

import (
	"encoding/json"
	"strings"
	"testing"

	oai "github.com/openai/openai-go/v3"
)

// TestToolCallChunksMatchSDK checks the tool stream shape OpenAI clients accumulate:
// content null, then a name fragment, then one arguments fragment, then finish_reason tool_calls.
func TestToolCallChunksMatchSDK(t *testing.T) {
	calls := []ToolResult{{ID: "call_abcdefghijklmnopqrstuvwx", Name: "get_weather", Arguments: `{"city":"北京"}`}}
	chunks := ToolCallChunks("chatcmpl-1", 10, "superllm", "", calls)
	if len(chunks) != 3 {
		t.Fatal(len(chunks))
	}
	raw0, _ := json.Marshal(chunks[0])
	raw1, _ := json.Marshal(chunks[1])
	raw2, _ := json.Marshal(chunks[2])
	for _, want := range []string{`"content":null`, `"role":"assistant"`, `"id":"call_abcdefghijklmnopqrstuvwx"`, `"name":"get_weather"`, `"arguments":""`} {
		if !strings.Contains(string(raw0), want) {
			t.Fatalf("missing %s in %s", want, raw0)
		}
	}
	if strings.Contains(string(raw1), `"name"`) || !strings.Contains(string(raw1), `"arguments":"{\"city\":\"北京\"}"`) {
		t.Fatal(string(raw1))
	}
	if !strings.Contains(string(raw2), `"finish_reason":"tool_calls"`) {
		t.Fatal(string(raw2))
	}
	var acc oai.ChatCompletionAccumulator
	for _, ch := range chunks {
		b, err := json.Marshal(ch)
		if err != nil {
			t.Fatal(err)
		}
		var parsed oai.ChatCompletionChunk
		if err := json.Unmarshal(b, &parsed); err != nil {
			t.Fatal(err, string(b))
		}
		if !acc.AddChunk(parsed) {
			t.Fatal("accumulator rejected", string(b))
		}
	}
	msg := acc.Choices[0].Message
	if acc.Choices[0].FinishReason != "tool_calls" || msg.ToolCalls[0].Function.Name != "get_weather" || msg.ToolCalls[0].Function.Arguments != `{"city":"北京"}` {
		t.Fatalf("%+v", msg)
	}
}

// TestFunctionCallChunksUseLegacyShape checks the deprecated function_call stream.
func TestFunctionCallChunksUseLegacyShape(t *testing.T) {
	chunks := FunctionCallChunks("chatcmpl-1", 10, "superllm", "checking", ToolResult{Name: "get_weather", Arguments: `{"city":"北京"}`})
	raw, _ := json.Marshal(chunks[0])
	if !strings.Contains(string(raw), `"content":"checking"`) || strings.Contains(string(raw), "tool_calls") {
		t.Fatal(string(raw))
	}
	body := NewCompletion("chatcmpl-1", 10, "superllm", "function_call", nil, nil, Usage{})
	body.Choices[0].Message.FunctionCall = &FunctionWire{Name: "get_weather", Arguments: `{"city":"北京"}`}
	encoded, _ := json.Marshal(body)
	if strings.Contains(string(encoded), "tool_calls") || !strings.Contains(string(encoded), `"function_call"`) || !strings.Contains(string(encoded), `"content":null`) {
		t.Fatal(string(encoded))
	}
}
