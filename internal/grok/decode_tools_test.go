package grok

import (
	"strings"
	"testing"
)

// TestDecodeToolResultOnce reads a partial tool_use, the assistant copy, and a
// string tool_result shaped like grok 1.0.41 image_gen. The same id is delivered once.
func TestDecodeToolResultOnce(t *testing.T) {
	const stream = `{"type":"system","subtype":"init","session_id":"sid","tools":["image_gen"]}
{"type":"stream_event","event":{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call-1","name":"image_gen","input":{}}},"session_id":"sid"}
{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"prompt\":\"A red apple\",\"aspect_ratio\":\"16:9\"}"}},"session_id":"sid"}
{"type":"stream_event","event":{"type":"content_block_stop","index":1},"session_id":"sid"}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"call-1","name":"image_gen","input":{"prompt":"A red apple","aspect_ratio":"16:9"}}]},"session_id":"sid"}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","is_error":false,"content":"{\"type\":\"ImageGen\",\"path\":\"/tmp/sessions/sid/images/1.jpg\"}"}]},"session_id":"sid"}
{"type":"result","subtype":"success","is_error":false,"result":"DONE","session_id":"sid"}
`
	var uses []ToolUse
	var results []ToolResult
	_, err := Decode(strings.NewReader(stream), Events{
		OnToolUse:    func(tu ToolUse) error { uses = append(uses, tu); return nil },
		OnToolResult: func(tr ToolResult) error { results = append(results, tr); return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(uses) != 1 || uses[0].Name != "image_gen" || !strings.Contains(string(uses[0].Input), "A red apple") {
		t.Fatalf("uses %#v", uses)
	}
	if len(results) != 1 || results[0].IsError || !strings.Contains(results[0].Text, "/tmp/sessions/sid/images/1.jpg") {
		t.Fatalf("results %#v", results)
	}
}

// TestDecodeInlineAndMaxTurns reads a block-array tool_result and error_max_turns.
func TestDecodeInlineAndMaxTurns(t *testing.T) {
	const inline = `{"type":"system","subtype":"init","tools":["image_gen"],"session_id":"sid"}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"c","is_error":false,"content":[{"type":"text","text":"no path"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="}}]}]}}
{"type":"result","subtype":"error_max_turns","is_error":true,"result":"max","session_id":"sid"}
`
	var got ToolResult
	_, err := Decode(strings.NewReader(inline), Events{OnToolResult: func(tr ToolResult) error { got = tr; return nil }})
	ge, ok := AsError(err)
	if !ok || ge.Code != CodeMaxTurns {
		t.Fatal(err)
	}
	if got.Text != "no path" || len(got.Inline) != 1 || string(got.Inline[0].Data) != "hi" {
		t.Fatalf("%#v", got)
	}
}

// TestClassifyMedia maps moderation, upgrade, and other tool errors.
func TestClassifyMedia(t *testing.T) {
	if ClassifyMedia("content policy violation: moderation").Code != CodeContentPolicy {
		t.Fatal("policy")
	}
	if ClassifyMedia("Please upgrade your subscription").Code != CodeMediaUnavailable {
		t.Fatal("tier")
	}
	if ClassifyMedia("zero data retention is enabled").Code != CodeMediaUnavailable {
		t.Fatal("zdr")
	}
	if ClassifyMedia("safety filter tripped").Code != CodeMediaFailed {
		t.Fatal("safety")
	}
}
