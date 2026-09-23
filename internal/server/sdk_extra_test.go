package server

import (
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// TestRejectsAndAuth covers 400s and bearer auth.
func TestRejectsAndAuth(t *testing.T) {
	h := start(t, startOpt{})
	status, _, body := h.postJSON(`{"model":"m","n":2,"messages":[{"role":"user","content":"x"}]}`, nil)
	assertCode(t, status, body, 400, "unsupported_parameter", "n")
	status, _, body = h.postJSON(`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"http://x"}}]}]}`, nil)
	assertCode(t, status, body, 400, "unsupported_content_part", "image_url")
	status, _, body = h.postJSON(`{"model":"m","reasoning_effort":"turbo","messages":[{"role":"user","content":"x"}]}`, nil)
	assertCode(t, status, body, 400, "unsupported_parameter", "reasoning_effort")
	status, _, body = h.postJSON(`{"model":"m","reasoning_effort":"low","messages":[{"role":"user","content":"x"}]}`, nil)
	if status != 200 {
		t.Fatal(status, string(body))
	}
	if !containsArg(h.userReports()[len(h.userReports())-1].Argv, "--reasoning-effort", "low") {
		t.Fatal(h.userReports()[len(h.userReports())-1].Argv)
	}

	ha := start(t, startOpt{apiKey: "s3cret"})
	req, _ := http.NewRequest(http.MethodPost, ha.ts.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"x"}]}`))
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	assertCode(t, res.StatusCode, b, 401, "invalid_api_key", "")
	req.Header.Set("Authorization", "Bearer wrong")
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(res.Body)
	res.Body.Close()
	assertCode(t, res.StatusCode, b, 401, "invalid_api_key", "")
	req.Header.Set("Authorization", "Bearer s3cret")
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatal(res.StatusCode, string(b))
	}
}

// TestErrorFixtures maps recorded grok failures onto the HTTP table.
func TestErrorFixtures(t *testing.T) {
	cases := []struct {
		scenario, code string
		status         int
		def            string
	}{
		{"not-logged-in", "grok_not_logged_in", 401, ""},
		{"bad-model", "model_not_found", 400, "no-such-model"},
		{"rate-limit", "grok_rate_limited", 429, ""},
		{"usage-limit", "grok_usage_limit", 429, ""},
		{"unsafe", "unsafe_grok_toolset", 500, ""},
	}
	for _, tc := range cases {
		t.Run(tc.scenario, func(t *testing.T) {
			o := startOpt{scenario: tc.scenario, defModel: tc.def}
			if tc.scenario == "not-logged-in" {
				o.login = "no"
			}
			h := start(t, o)
			status, _, body := h.postJSON(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`, nil)
			assertCode(t, status, body, tc.status, tc.code, "")
			if tc.code == "grok_not_logged_in" && !strings.Contains(string(body), "grok login") {
				t.Fatal(string(body))
			}
		})
	}
}

// TestAccessLog writes one line and hides the prompt unless -log-prompts is set.
func TestAccessLog(t *testing.T) {
	h := start(t, startOpt{})
	secret := "super-secret-prompt-text"
	status, _, _ := h.postJSON(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"`+secret+`"}]}`, nil)
	if status != 200 {
		t.Fatal(status)
	}
	logs := strings.Split(strings.TrimSpace(h.log.String()), "\n")
	var chat []string
	for _, line := range logs {
		if strings.Contains(line, "POST /v1/chat/completions") {
			chat = append(chat, line)
		}
	}
	if len(chat) != 1 {
		t.Fatal(h.log.String())
	}
	line := chat[0]
	for _, s := range []string{"POST /v1/chat/completions", "model=", "stream=false", "tools=0", "status=200", "dur=", "in=", "out="} {
		if !strings.Contains(line, s) {
			t.Fatal(line)
		}
	}
	if strings.Contains(line, secret) {
		t.Fatal(line)
	}
	h2 := start(t, startOpt{logPrompts: true})
	h2.postJSON(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"`+secret+`"}]}`, nil)
	if !strings.Contains(h2.log.String(), secret) {
		t.Fatal(h2.log.String())
	}
}

// TestSessionDelete asks fakegrok to delete the session unless -keep-sessions is set.
func TestSessionDelete(t *testing.T) {
	h := start(t, startOpt{})
	h.postJSON(`{"model":"m","messages":[{"role":"user","content":"x"}]}`, nil)
	waitFor(t, 2*time.Second, func() bool {
		b, _ := os.ReadFile(h.dir + "/deletes")
		return strings.TrimSpace(string(b)) != ""
	})
	hk := start(t, startOpt{keep: true})
	hk.postJSON(`{"model":"m","messages":[{"role":"user","content":"x"}]}`, nil)
	time.Sleep(200 * time.Millisecond)
	b, _ := os.ReadFile(hk.dir + "/deletes")
	if strings.TrimSpace(string(b)) != "" {
		t.Fatal(string(b))
	}
}
