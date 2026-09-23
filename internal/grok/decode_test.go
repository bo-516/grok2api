package grok

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture opens a recorded grok 1.0.41 stdout file.
func fixture(t *testing.T, name string) *os.File {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "testdata", "grok", name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// TestDecodeTextJSON reads the pretty-printed output-format json capture.
func TestDecodeTextJSON(t *testing.T) {
	final, err := Decode(fixture(t, "text.json"), Events{})
	if err != nil {
		t.Fatal(err)
	}
	if final.Text != "pong" || final.StopReason != "end_turn" {
		t.Fatalf("%+v", final)
	}
	if final.Model != "grok-4.7-build-fast" {
		t.Fatal(final.Model)
	}
	if final.Usage.PromptTokens() != 3187+1152 || final.Usage.CompletionTokens() != 38 || !final.Usage.ReasoningSet || final.Usage.Reasoning != 37 {
		t.Fatalf("%+v", final.Usage)
	}
	if final.SessionID == "" || strings.Contains(final.Text, "The user wants") {
		t.Fatalf("thinking leaked or missing session: %+v", final)
	}
}

// TestDecodeStream checks text deltas, an empty tool list, and the final reply.
func TestDecodeStream(t *testing.T) {
	var tools string
	var deltas strings.Builder
	final, err := Decode(fixture(t, "stream.ndjson"), Events{
		OnInit: func(info Init) error {
			tools = string(info.ToolsRaw)
			if info.Model != "grok-4.7-build-fast" {
				t.Fatal(info.Model)
			}
			return nil
		},
		OnText: func(d string) error {
			deltas.WriteString(d)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if tools != "[]" {
		t.Fatal(tools)
	}
	if final.Text != "1, 2, 3, 4, 5" || deltas.String() != final.Text {
		t.Fatalf("text %q deltas %q", final.Text, deltas.String())
	}
	if strings.Contains(final.Text, "The user wants") {
		t.Fatal("thinking leaked")
	}
}

// TestDecodeSchema checks that --json-schema still streamed and reported structured_output.
func TestDecodeSchema(t *testing.T) {
	var sawInit bool
	final, err := Decode(fixture(t, "schema.ndjson"), Events{OnInit: func(info Init) error {
		sawInit = ToolsEmpty(info.ToolsRaw)
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !sawInit || final.Text != `{"city":"Paris"}` || !strings.Contains(string(final.Structured), "Paris") {
		t.Fatalf("init %v text %s structured %s", sawInit, final.Text, final.Structured)
	}
}

// TestDecodeTools reads the anyOf envelope capture.
func TestDecodeTools(t *testing.T) {
	final, err := Decode(fixture(t, "tools.ndjson"), Events{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(final.Structured), "get_weather") || !strings.Contains(string(final.Structured), "Beijing") {
		t.Fatal(string(final.Structured))
	}
}

// TestDecodeErrors classifies recorded failure stdout.
func TestDecodeErrors(t *testing.T) {
	_, err := Decode(fixture(t, "bad-model.ndjson"), Events{})
	ge, ok := AsError(err)
	if !ok || ge.Code != CodeModelNotFound {
		t.Fatal(err)
	}
	_, err = Decode(fixture(t, "not-logged-in.ndjson"), Events{})
	ge, ok = AsError(err)
	if !ok || ge.Code != CodeNotLoggedIn || !strings.Contains(ge.Message, "grok login") {
		t.Fatal(err)
	}
	_, err = Decode(fixture(t, "rate-limit.json"), Events{})
	ge, ok = AsError(err)
	if !ok || ge.Code != CodeRateLimited {
		t.Fatal(err)
	}
	_, err = Decode(fixture(t, "usage-limit.json"), Events{})
	ge, ok = AsError(err)
	if !ok || ge.Code != CodeUsageLimit {
		t.Fatal(err)
	}
}

// TestDecodeResultWithoutInit rejects a streaming result that never sent system/init.
// A bare text.json object has no type field and stays valid.
func TestDecodeResultWithoutInit(t *testing.T) {
	_, err := Decode(strings.NewReader("{\"type\":\"result\",\"result\":\"pong\",\"stop_reason\":\"end_turn\",\"session_id\":\"abc-def\"}\n"), Events{})
	ge, ok := AsError(err)
	if !ok || ge.Code != CodeUnsafe {
		t.Fatal(err)
	}
}

// TestDecodeTruncatedAfterDelta refuses a cut-off value even after a text delta.
func TestDecodeTruncatedAfterDelta(t *testing.T) {
	in := "{\"type\":\"system\",\"subtype\":\"init\",\"tools\":[],\"session_id\":\"s\",\"model\":\"m\"}\n" +
		"{\"type\":\"stream_event\",\"event\":{\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}}\n" +
		"{"
	final, err := Decode(strings.NewReader(in), Events{})
	ge, ok := AsError(err)
	if !ok || ge.Code != CodeBadOutput {
		t.Fatalf("%v text %q", err, final.Text)
	}
	if final.Text != "hello" {
		t.Fatal(final.Text)
	}
}

// TestDecodeFollowup reads the recorded tool-result reply.
func TestDecodeFollowup(t *testing.T) {
	final, err := Decode(fixture(t, "followup.ndjson"), Events{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(final.Text, "20") {
		t.Fatal(final.Text)
	}
}
