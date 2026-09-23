package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/shared"
)

// TestNonStreamText drives Chat.Completions.New against the text fixture.
func TestNonStreamText(t *testing.T) {
	h := start(t, startOpt{})
	want := fixtureText(t, "text.json")
	resp, err := h.completions().New(context.Background(), openai.ChatCompletionNewParams{
		Model: "gpt-4o-mini",
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("Reply with exactly the word: pong"),
		},
		Temperature: openai.Float(0.2),
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Object != "chat.completion" || resp.Choices[0].FinishReason != "stop" || resp.Choices[0].Message.Content != want {
		t.Fatalf("%s %s %q", resp.Object, resp.Choices[0].FinishReason, resp.Choices[0].Message.Content)
	}
	if resp.Model != "grok-4.7-build-fast" || resp.Usage.TotalTokens <= 0 || resp.Usage.CompletionTokensDetails.ReasoningTokens <= 0 {
		t.Fatalf("model %s usage %+v", resp.Model, resp.Usage)
	}
	if !strings.HasPrefix(resp.ID, "chatcmpl-") || strings.Contains(strings.TrimPrefix(resp.ID, "chatcmpl-"), "-") {
		t.Fatal(resp.ID)
	}
	status, hdr, body := h.postJSON(`{"model":"gpt-4o-mini","temperature":0.2,"max_tokens":16,"messages":[{"role":"user","content":"Reply with exactly the word: pong"}]}`, nil)
	if status != 200 {
		t.Fatalf("%d %s", status, body)
	}
	if hdr.Get("X-Agent-Mock-Ignored") == "" || !strings.Contains(hdr.Get("X-Agent-Mock-Ignored"), "temperature") || !strings.Contains(hdr.Get("X-Agent-Mock-Ignored"), "max_tokens") {
		t.Fatal(hdr.Get("X-Agent-Mock-Ignored"))
	}
	if hdr.Get("X-Agent-Mock-Session") == "" {
		t.Fatal("missing session header")
	}
	reps := h.userReports()
	if len(reps) < 1 {
		t.Fatal("no report")
	}
	last := reps[len(reps)-1]
	if last.XAI {
		t.Fatal("XAI_API_KEY leaked")
	}
	if last.Mode != 0o600 {
		t.Fatalf("prompt mode %o", last.Mode)
	}
	assertRestricted(t, last.Argv)
	if hasFlag(last.Argv, "-m") {
		t.Fatalf("unknown model should omit -m: %v", last.Argv)
	}
}

// TestStreamSDK checks role, concatenated text, finish, usage, and [DONE].
func TestStreamSDK(t *testing.T) {
	h := start(t, startOpt{scenario: "stream"})
	want := fixtureText(t, "stream.ndjson")
	stream := h.completions().NewStreaming(context.Background(), openai.ChatCompletionNewParams{
		Model: "gpt-4o-mini",
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("Count"),
		},
		StreamOptions: openai.ChatCompletionStreamOptionsParam{IncludeUsage: openai.Bool(true)},
	})
	var text strings.Builder
	var role, finish, usage bool
	for stream.Next() {
		ch := stream.Current()
		if len(ch.Choices) == 0 {
			if ch.Usage.TotalTokens > 0 {
				usage = true
			}
			continue
		}
		if ch.Choices[0].Delta.Role == "assistant" {
			role = true
		}
		text.WriteString(ch.Choices[0].Delta.Content)
		if ch.Choices[0].FinishReason == "stop" {
			finish = true
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if !role || !finish || !usage || text.String() != want {
		t.Fatalf("role %v finish %v usage %v text %q", role, finish, usage, text.String())
	}
	raw := rawStream(t, h, `{"model":"gpt-4o-mini","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"Count"}]}`)
	if !strings.Contains(raw, "data: [DONE]") {
		t.Fatal(raw)
	}
	if !strings.HasPrefix(strings.TrimSpace(raw), "data: ") {
		t.Fatal(raw[:80])
	}
}

// TestSchemaAndTools covers JSON mode, tool calls, and the tool-result follow-up.
func TestSchemaAndTools(t *testing.T) {
	h := start(t, startOpt{scenario: "schema"})
	resp, err := h.completions().New(context.Background(), openai.ChatCompletionNewParams{
		Model: "gpt-4o-mini",
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("Capital of France?"),
		},
		ResponseFormat: openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{
				JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
					Name:   "capital",
					Schema: map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}, "required": []any{"city"}},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var city struct {
		City string `json:"city"`
	}
	if err := json.Unmarshal([]byte(resp.Choices[0].Message.Content), &city); err != nil || city.City != "Paris" {
		t.Fatal(resp.Choices[0].Message.Content, err)
	}
	if !hasFlag(h.userReports()[0].Argv, "--json-schema") || !containsArg(h.userReports()[0].Argv, "--output-format", "streaming-messages-json") {
		t.Fatal(h.userReports()[0].Argv)
	}

	h2 := start(t, startOpt{scenario: "tools,followup"})
	toolResp, err := h2.completions().New(context.Background(), openai.ChatCompletionNewParams{
		Model: "gpt-4o-mini",
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("北京今天天气怎么样？"),
		},
		Tools: []openai.ChatCompletionToolUnionParam{
			openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
				Name:        "get_weather",
				Description: openai.String("weather"),
				Parameters: shared.FunctionParameters{
					"type":       "object",
					"properties": map[string]any{"city": map[string]any{"type": "string"}},
					"required":   []any{"city"},
				},
			}),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if toolResp.Choices[0].FinishReason != "tool_calls" || len(toolResp.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("%+v", toolResp.Choices[0])
	}
	call := toolResp.Choices[0].Message.ToolCalls[0]
	if call.Function.Name != "get_weather" || !strings.Contains(call.Function.Arguments, "Beijing") {
		t.Fatalf("%+v", call)
	}
	if !strings.HasPrefix(call.ID, "call_") || len(call.ID) != len("call_")+24 {
		t.Fatal(call.ID)
	}
	follow, err := h2.completions().New(context.Background(), openai.ChatCompletionNewParams{
		Model: "gpt-4o-mini",
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("北京今天天气怎么样？"),
			toolResp.Choices[0].Message.ToParam(),
			openai.ToolMessage(`{"temp_c":20}`, call.ID),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := fixtureText(t, "followup.ndjson")
	if follow.Choices[0].FinishReason != "stop" || follow.Choices[0].Message.Content != want || !strings.Contains(follow.Choices[0].Message.Content, "20") {
		t.Fatalf("%s %q", follow.Choices[0].FinishReason, follow.Choices[0].Message.Content)
	}
	reps := h2.userReports()
	if len(reps) < 2 || !strings.Contains(reps[1].Body, `{"temp_c":20}`) || !strings.Contains(reps[1].Body, call.ID) {
		t.Fatalf("follow-up prompt not rendered: %+v", reps)
	}

	h3 := start(t, startOpt{scenario: "tools"})
	st := h3.completions().NewStreaming(context.Background(), openai.ChatCompletionNewParams{
		Model:    "gpt-4o-mini",
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("北京今天天气怎么样？")},
		Tools: []openai.ChatCompletionToolUnionParam{
			openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{Name: "get_weather", Parameters: shared.FunctionParameters{"type": "object"}}),
		},
	})
	var acc openai.ChatCompletionAccumulator
	for st.Next() {
		if !acc.AddChunk(st.Current()) {
			t.Fatal("accumulator rejected chunk")
		}
	}
	if err := st.Err(); err != nil {
		t.Fatal(err)
	}
	if acc.Choices[0].FinishReason != "tool_calls" || acc.Choices[0].Message.ToolCalls[0].Function.Name != "get_weather" {
		t.Fatalf("%+v", acc.Choices)
	}
}

// TestModelMap passes known models through, applies aliases, and omits -m otherwise.
func TestModelMap(t *testing.T) {
	h := start(t, startOpt{modelMap: map[string]string{"gpt-4o": "grok-4.7"}, scenario: "text"})
	mustComplete(t, h, "gpt-4o")
	mustComplete(t, h, "grok-4.6")
	mustComplete(t, h, "foo")
	reps := h.userReports()
	if len(reps) < 3 {
		t.Fatal(len(reps))
	}
	if !containsArg(reps[0].Argv, "-m", "grok-4.7") {
		t.Fatal(reps[0].Argv)
	}
	if !containsArg(reps[1].Argv, "-m", "grok-4.6") {
		t.Fatal(reps[1].Argv)
	}
	if hasFlag(reps[2].Argv, "-m") {
		t.Fatal(reps[2].Argv)
	}
	resp, err := h.completions().New(context.Background(), openai.ChatCompletionNewParams{
		Model:    "gpt-4o",
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("x")},
	})
	if err != nil || resp.Model != "grok-4.7" {
		t.Fatal(err, resp)
	}
}

// TestModelsAndHealth lists grok models, aliases, and the health payload.
func TestModelsAndHealth(t *testing.T) {
	h := start(t, startOpt{modelMap: map[string]string{"gpt-4o": "grok-4.7"}})
	res, err := http.Get(h.ts.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var list struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.NewDecoder(res.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if list.Object != "list" {
		t.Fatal(list.Object)
	}
	foundModel, foundAlias := false, false
	for _, d := range list.Data {
		if d.ID == "grok-4.7-build-fast" && d.OwnedBy == "xai" {
			foundModel = true
		}
		if d.ID == "gpt-4o" && d.OwnedBy == "agent-mock-alias" {
			foundAlias = true
		}
	}
	if !foundModel || !foundAlias {
		t.Fatal(list.Data)
	}
	hr, err := http.Get(h.ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer hr.Body.Close()
	var health map[string]any
	if err := json.NewDecoder(hr.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	if hr.StatusCode != 200 || health["login"] != "ok" || health["grok"] != "1.0.41" {
		t.Fatalf("%d %+v", hr.StatusCode, health)
	}
}

func mustComplete(t *testing.T, h *harness, model string) {
	t.Helper()
	_, err := h.completions().New(context.Background(), openai.ChatCompletionNewParams{
		Model:    model,
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("x")},
	})
	if err != nil {
		t.Fatal(model, err)
	}
}

func assertCode(t *testing.T, status int, body []byte, wantStatus int, code, field string) {
	t.Helper()
	var env struct {
		Error struct {
			Code, Message string
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err, string(body))
	}
	if status != wantStatus || env.Error.Code != code || (field != "" && !strings.Contains(env.Error.Message, field)) {
		t.Fatalf("status %d code %s msg %s", status, env.Error.Code, env.Error.Message)
	}
}

func assertRestricted(t *testing.T, argv []string) {
	t.Helper()
	joined := strings.Join(argv, "\n")
	for _, s := range []string{"--verbatim", "--prompt-file", "streaming-messages-json", "--include-partial-messages", "--max-turns", "--no-subagents", "--no-plan", "--disable-web-search", "--permission-mode", "dontAsk", "--tools", "todo_write", "--disallowed-tools", "search_tool,use_tool,todo_write", "--cwd"} {
		if !strings.Contains(joined, s) {
			t.Fatalf("missing %s in %s", s, joined)
		}
	}
	if strings.Contains(joined, "bypassPermissions") {
		t.Fatal(joined)
	}
}

func hasFlag(argv []string, name string) bool {
	for _, a := range argv {
		if a == name {
			return true
		}
	}
	return false
}

func containsArg(argv []string, name, val string) bool {
	for i, a := range argv {
		if a == name && i+1 < len(argv) && argv[i+1] == val {
			return true
		}
	}
	return false
}

func rawStream(t *testing.T, h *harness, body string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.ts.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	return string(b)
}

func waitFor(t *testing.T, d time.Duration, ok func() bool) {
	t.Helper()
	dead := time.Now().Add(d)
	for time.Now().Before(dead) {
		if ok() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met")
}
