//go:build live

// Package e2e runs agent-mock against the logged-in grok on this machine.
package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

// callID matches the tool-call ids agent-mock generates.
var callID = regexp.MustCompile(`^call_[A-Za-z0-9]{24}$`)

// TestLive is the real-grok acceptance path. It is skipped unless AGENT_MOCK_LIVE=1.
func TestLive(t *testing.T) {
	if os.Getenv("AGENT_MOCK_LIVE") != "1" {
		t.Skip("set AGENT_MOCK_LIVE=1")
	}
	if _, err := exec.LookPath("grok"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	srv := startServer(t, ctx, nil)
	if !strings.Contains(srv.banner, "toolset   []") || !strings.Contains(srv.banner, "permission-mode=dontAsk") {
		t.Fatalf("startup banner:\n%s", srv.banner)
	}
	client := openai.NewClient(option.WithBaseURL(srv.base), option.WithAPIKey("dev"), option.WithMaxRetries(0))

	schema := mustCity(t, ctx, client)
	toolID := mustWeather(t, ctx, client)
	canaryID := mustCanary(t, ctx, client)
	for _, id := range []string{schema, toolID, canaryID} {
		if id == "" {
			t.Fatal("empty session")
		}
		waitUntil(t, 20*time.Second, func() bool { return !sessionListed(t, id) })
	}

	kept := startServer(t, ctx, []string{"-keep-sessions"})
	keptClient := openai.NewClient(option.WithBaseURL(kept.base), option.WithAPIKey("dev"), option.WithMaxRetries(0))
	keptID := sessionOf(t, mustText(t, ctx, keptClient))
	waitUntil(t, 15*time.Second, func() bool { return sessionListed(t, keptID) })
}

type running struct {
	base   string
	banner string
	cmd    *exec.Cmd
}

// startServer launches the real agent-mock binary and waits for the startup banner.
func startServer(t *testing.T, ctx context.Context, extra []string) *running {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	bin := buildBinary(t)
	args := append([]string{"-addr", addr, "-grok-bin", "grok"}, extra...)
	cmd := exec.CommandContext(ctx, bin, args...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { _, _ = cmd.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
		}
	})
	ready := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stderr)
		var b strings.Builder
		sent := false
		for sc.Scan() {
			b.WriteString(sc.Text())
			b.WriteByte('\n')
			if !sent && strings.Contains(b.String(), "OPENAI_BASE_URL=") && strings.Contains(b.String(), "toolset") {
				ready <- b.String()
				sent = true
			}
		}
	}()
	select {
	case banner := <-ready:
		return &running{base: "http://" + addr + "/v1", banner: banner, cmd: cmd}
	case <-time.After(15 * time.Second):
		t.Fatal("startup banner was not ready within 15s")
	}
	return nil
}

// buildBinary compiles cmd/agent-mock into a temp directory.
func buildBinary(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "agent-mock")
	cmd := exec.Command("go", "build", "-o", out, "./cmd/agent-mock")
	cmd.Dir = moduleRoot(t)
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatal(string(b), err)
	}
	return out
}

// moduleRoot is the repo root, two levels above this file's package when tests run
// with the package directory as the working directory... tests run in e2e/, so root is "..".
func moduleRoot(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

// mustCity checks a json_schema completion and returns the grok session id.
func mustCity(t *testing.T, ctx context.Context, client openai.Client) string {
	t.Helper()
	resp, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model:    "gpt-4o-mini",
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("What is the capital of France?")},
		ResponseFormat: openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{
				JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
					Name: "capital",
					Schema: map[string]any{
						"type": "object", "additionalProperties": false, "required": []any{"city"},
						"properties": map[string]any{"city": map[string]any{"type": "string"}},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		City string `json:"city"`
	}
	if json.Unmarshal([]byte(resp.Choices[0].Message.Content), &body) != nil || body.City != "Paris" {
		t.Fatal(resp.Choices[0].Message.Content)
	}
	return sessionFromID(resp.ID)
}

// mustWeather checks a get_weather tool call and a follow-up that mentions 20.
// It returns the first call's session id. The follow-up session is also checked by the caller
// only for the three ids it is given; this function returns the tool-call session.
func mustWeather(t *testing.T, ctx context.Context, client openai.Client) string {
	t.Helper()
	resp, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model:    "gpt-4o-mini",
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("北京今天天气怎么样？")},
		Tools: []openai.ChatCompletionToolUnionParam{
			openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
				Name:        "get_weather",
				Description: openai.String("Weather for a city. Copy the city exactly as the user wrote it. Do not translate."),
				Parameters: shared.FunctionParameters{
					"type": "object", "additionalProperties": false, "required": []any{"city"},
					"properties": map[string]any{"city": map[string]any{"type": "string"}},
				},
			}),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Choices[0].FinishReason != "tool_calls" || len(resp.Choices[0].Message.ToolCalls) == 0 {
		t.Fatalf("%+v", resp.Choices[0])
	}
	call := resp.Choices[0].Message.ToolCalls[0]
	if call.Function.Name != "get_weather" || !strings.Contains(call.Function.Arguments, "北京") || !callID.MatchString(call.ID) {
		t.Fatalf("%s %s %s", call.ID, call.Function.Name, call.Function.Arguments)
	}
	follow, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model: "gpt-4o-mini",
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("北京今天天气怎么样？"),
			resp.Choices[0].Message.ToParam(),
			openai.ToolMessage(`{"temp_c":20}`, call.ID),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if follow.Choices[0].FinishReason != "stop" || !strings.Contains(follow.Choices[0].Message.Content, "20") {
		t.Fatal(follow.Choices[0].FinishReason, follow.Choices[0].Message.Content)
	}
	return sessionFromID(resp.ID)
}

// mustCanary asks grok to touch a file and checks the file was not created.
func mustCanary(t *testing.T, ctx context.Context, client openai.Client) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "canary")
	resp := mustRaw(t, ctx, client, "Use the shell to run this exact command: touch "+path)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("canary exists", err)
	}
	return sessionFromID(resp.ID)
}

// mustText is a short completion used for the keep-sessions check.
func mustText(t *testing.T, ctx context.Context, client openai.Client) *openai.ChatCompletion {
	t.Helper()
	return mustRaw(t, ctx, client, "Reply with exactly the word: pong")
}

// mustRaw sends one user message.
func mustRaw(t *testing.T, ctx context.Context, client openai.Client, content string) *openai.ChatCompletion {
	t.Helper()
	resp, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model:    "gpt-4o-mini",
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage(content)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// sessionOf reads the session id from a completion id.
func sessionOf(t *testing.T, resp *openai.ChatCompletion) string {
	t.Helper()
	return sessionFromID(resp.ID)
}

// sessionFromID turns chatcmpl-<32 hex> back into a hyphenated session id when it looks like one.
// grok session ids are UUIDs. The completion id strips hyphens, so this restores 8-4-4-4-12.
func sessionFromID(id string) string {
	raw := strings.TrimPrefix(id, "chatcmpl-")
	if len(raw) != 32 {
		return raw
	}
	return raw[0:8] + "-" + raw[8:12] + "-" + raw[12:16] + "-" + raw[16:20] + "-" + raw[20:32]
}

// sessionListed reports whether grok's session list for the shared empty cwd still shows id.
// `grok sessions list` without --cwd only shows the caller's directory, so headless runs
// in agent-mock's fixed cwd are invisible unless --cwd points there. Q-3 showed delete
// itself is non-interactive; the list must use the same directory the runs used.
func sessionListed(t *testing.T, id string) bool {
	t.Helper()
	cwd := filepath.Join(os.TempDir(), "agent-mock", "cwd")
	cmd := exec.Command("grok", "--cwd", cwd, "sessions", "list", "-n", "50")
	b, err := cmd.Output()
	if err != nil {
		t.Fatal(err, string(b))
	}
	return strings.Contains(string(b), id)
}

// waitUntil polls until ok is true or the deadline passes.
func waitUntil(t *testing.T, d time.Duration, ok func() bool) {
	t.Helper()
	dead := time.Now().Add(d)
	for time.Now().Before(dead) {
		if ok() {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("condition not met")
}
