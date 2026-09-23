package server

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestQueue returns 429 agent_mock_busy with Retry-After when the slot is held.
func TestQueue(t *testing.T) {
	h := start(t, startOpt{maxConc: 1, queue: time.Second, extra: []string{"FAKEGROK_START_DELAY=3s"}})
	var wg sync.WaitGroup
	type result struct {
		status int
		body   string
		at     time.Duration
	}
	ch := make(chan result, 2)
	start := time.Now()
	body := `{"model":"m","messages":[{"role":"user","content":"x"}]}`
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, hdr, b := h.postJSON(body, nil)
			ch <- result{status, string(b), time.Since(start)}
			if status == 429 && hdr.Get("Retry-After") != "5" {
				t.Errorf("Retry-After %q", hdr.Get("Retry-After"))
			}
		}()
	}
	wg.Wait()
	close(ch)
	var ok, busy int
	for r := range ch {
		if r.status == 200 {
			ok++
		}
		if r.status == 429 && strings.Contains(r.body, "agent_mock_busy") && r.at < 1500*time.Millisecond {
			busy++
		}
	}
	if ok != 1 || busy != 1 {
		t.Fatalf("ok %d busy %d", ok, busy)
	}
}

// TestRequestTimeout returns 504 when fakegrok ignores SIGTERM and sleeps.
func TestRequestTimeout(t *testing.T) {
	h := start(t, startOpt{scenario: "hang", reqTimeout: 2 * time.Second})
	start := time.Now()
	status, _, body := h.postJSON(`{"model":"m","messages":[{"role":"user","content":"x"}]}`, nil)
	if time.Since(start) > 8*time.Second {
		t.Fatal("too slow", time.Since(start))
	}
	assertCode(t, status, body, 504, "grok_timeout", "")
}

// TestCancelKillsGroup removes the prompt file and the fakegrok pid after the client cancels.
func TestCancelKillsGroup(t *testing.T) {
	h := start(t, startOpt{scenario: "hang", reqTimeout: time.Minute})
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.ts.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"cancel-me"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	done := make(chan struct{})
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		_ = err
		close(done)
	}()
	time.Sleep(300 * time.Millisecond)
	pidRaw, err := os.ReadFile(filepath.Join(h.dir, "pid"))
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	_, _ = fmtSscanf(string(pidRaw), &pid)
	reps := h.userReports()
	if len(reps) == 0 || reps[len(reps)-1].Prompt == "" {
		t.Fatal("missing prompt path")
	}
	promptPath := reps[len(reps)-1].Prompt
	time.Sleep(700 * time.Millisecond)
	cancel()
	<-done
	canceled := time.Now()
	waitFor(t, 6*time.Second, func() bool {
		return syscall.Kill(pid, 0) != nil
	})
	if time.Since(canceled) > 6*time.Second {
		t.Fatal("pid still alive")
	}
	waitFor(t, 2*time.Second, func() bool {
		_, err := os.Stat(promptPath)
		return os.IsNotExist(err)
	})
}

// TestFlushBeforeDone returns the role chunk while fakegrok is still holding the rest.
func TestFlushBeforeDone(t *testing.T) {
	h := start(t, startOpt{scenario: "stream", extra: []string{"FAKEGROK_HOLD=2s"}})
	req, err := http.NewRequest(http.MethodPost, h.ts.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"Count"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(line, "data:") {
			if time.Since(start) > time.Second {
				t.Fatal("first chunk waited for the fixture", time.Since(start))
			}
			return
		}
	}
}

// TestKeepalive emits a comment about 15s after the stream goes idle.
func TestKeepalive(t *testing.T) {
	h := start(t, startOpt{scenario: "stream", extra: []string{"FAKEGROK_HOLD=20s"}})
	req, err := http.NewRequest(http.MethodPost, h.ts.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"Count"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	var first time.Time
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if first.IsZero() && strings.HasPrefix(line, "data:") {
			first = time.Now()
		}
		if strings.Contains(line, "keepalive") {
			d := time.Since(first)
			if d < 14*time.Second || d > 16*time.Second {
				t.Fatal(d)
			}
			return
		}
		if !first.IsZero() && time.Since(first) > 17*time.Second {
			t.Fatal("no keepalive")
		}
	}
}

// TestNoInit returns 502 when stdout stays empty for 20s.
func TestNoInit(t *testing.T) {
	h := start(t, startOpt{scenario: "silent", reqTimeout: time.Minute})
	start := time.Now()
	status, _, body := h.postJSON(`{"model":"m","messages":[{"role":"user","content":"x"}]}`, nil)
	if time.Since(start) > 24*time.Second {
		t.Fatal(time.Since(start))
	}
	assertCode(t, status, body, 502, "grok_failed", "")
}

// TestStreamErrorAfterHeaders writes one error event and no [DONE].
func TestStreamErrorAfterHeaders(t *testing.T) {
	h := start(t, startOpt{scenario: "stream-error"})
	raw := rawStream(t, h, `{"model":"m","stream":true,"messages":[{"role":"user","content":"x"}]}`)
	if !strings.Contains(raw, `"code":"grok_rate_limited"`) {
		t.Fatal(raw)
	}
	if strings.Contains(raw, "[DONE]") {
		t.Fatal(raw)
	}
}

// TestNoInitStreamingIsUnsafe rejects NDJSON whose first record is not system/init.
// The text.json object, which has no type field, remains a successful completion.
func TestNoInitStreamingIsUnsafe(t *testing.T) {
	h := start(t, startOpt{scenario: "no-init"})
	status, _, body := h.postJSON(`{"model":"m","messages":[{"role":"user","content":"x"}]}`, nil)
	assertCode(t, status, body, 500, "unsafe_grok_toolset", "system/init")
	if strings.Contains(string(body), "chat.completion") {
		t.Fatal(string(body))
	}
	pid := pidFrom(t, h.dir)
	if syscall.Kill(pid, 0) == nil {
		t.Fatal("grok process still alive")
	}
}

// TestTruncatedStdoutIsBadOutput does not turn a partial delta plus broken JSON into HTTP 200.
func TestTruncatedStdoutIsBadOutput(t *testing.T) {
	h := start(t, startOpt{scenario: "truncated"})
	status, _, body := h.postJSON(`{"model":"m","messages":[{"role":"user","content":"x"}]}`, nil)
	assertCode(t, status, body, 502, "grok_bad_output", "")
	if strings.Contains(string(body), "chat.completion") || strings.Contains(string(body), `"content":"hello"`) {
		t.Fatal(string(body))
	}
}

// pidFrom reads the fakegrok pid file written for this harness.
func pidFrom(t *testing.T, dir string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "pid"))
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	_, _ = fmtSscanf(string(b), &pid)
	if pid == 0 {
		t.Fatal("pid")
	}
	return pid
}

// TestBodyTooLarge returns 413.
func TestBodyTooLarge(t *testing.T) {
	h := start(t, startOpt{})
	payload := `{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("a", 8<<20) + `"}]}`
	status, _, body := h.postJSON(payload, nil)
	assertCode(t, status, body, 413, "request_too_large", "")
}

// TestBinaryNonLoopback exits 2 before listening when -api-key is missing.
func TestBinaryNonLoopback(t *testing.T) {
	cmd := exec.Command(agentBin, "-addr", "0.0.0.0:9")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		t.Fatal("expected exit")
	}
	var exit *exec.ExitError
	if !errorsAs(err, &exit) || exit.ExitCode() != 2 || !strings.Contains(stderr.String(), "-api-key is required when -addr is not loopback") {
		t.Fatal(err, stderr.String())
	}
	cmd = exec.Command(agentBin, "-version")
	out, err := cmd.Output()
	if err != nil || !strings.Contains(string(out), "agent-mock v0.1.0") {
		t.Fatal(err, string(out))
	}
	cmd = exec.Command(agentBin, "-h")
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatal(err, string(out))
	}
	for _, flag := range []string{"-addr", "-api-key", "-grok-bin", "-default-model", "-model-map", "-reasoning-effort", "-max-concurrency", "-queue-timeout", "-request-timeout", "-keep-sessions", "-log-prompts", "-version"} {
		if !strings.Contains(string(out), flag) {
			t.Fatal("help missing", flag, string(out))
		}
	}
}

func fmtSscanf(s string, pid *int) (int, error) {
	s = strings.TrimSpace(s)
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	*pid = n
	if n == 0 {
		return 0, io.EOF
	}
	return 1, nil
}

func errorsAs(err error, target **exec.ExitError) bool {
	if err == nil {
		return false
	}
	e, ok := err.(*exec.ExitError)
	if !ok {
		return false
	}
	*target = e
	return true
}
