package server

import (
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/shaoboli/agent-mock/internal/grok"
)

// TestShutdownFinishesDeletes starts the real agent-mock binary on fakegrok, makes
// one request, and sends SIGINT to agent-mock's whole process group, as a Ctrl-C in
// its terminal does. The session deletes of the startup probe and of the request must
// still finish before the process exits: before the fix the delete goroutine was
// dropped at exit and the `grok sessions delete` child died with the group.
// Afterwards GROK_HOME holds no session, no prompt_history.jsonl, and the temp root
// no pending record.
func TestShutdownFinishesDeletes(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	cmd := exec.Command(agentBin, "-addr", addr, "-grok-bin", fakeBin)
	cmd.Env = append(filteredEnv(),
		"TMPDIR="+tmp,
		"GROK_HOME="+filepath.Join(dir, "grok-home"),
		"FAKEGROK_FIXTURE_DIR="+fixDir,
		"FAKEGROK_SCENARIO=text",
		"FAKEGROK_DELETE_LOG="+filepath.Join(dir, "deletes"),
		"FAKEGROK_DELETE_DELAY=1500ms",
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stderr := &lockBuf{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
	waitFor(t, 15*time.Second, func() bool { return strings.Contains(stderr.String(), "OPENAI_BASE_URL=") })

	resp, err := http.Post("http://"+addr+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"x"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	id := resp.Header.Get("X-Agent-Mock-Session")
	if resp.StatusCode != http.StatusOK || len(id) != 36 {
		t.Fatalf("status %s session %q\n%s", resp.Status, id, stderr.String())
	}
	if err := syscall.Kill(-pgid, syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(12 * time.Second):
		t.Fatal("agent-mock did not exit within its 10 s shutdown budget")
	}

	b, _ := os.ReadFile(filepath.Join(dir, "deletes"))
	// Chat probe, media probe, and the request each delete one session.
	if lines := strings.Fields(string(b)); len(lines) != 3 || !strings.Contains(string(b), id) {
		t.Fatalf("want the chat probe, the media probe, and %s deleted before exit, got %q\n%s", id, b, stderr.String())
	}
	if dirs, _ := filepath.Glob(filepath.Join(dir, "grok-home", "sessions", "*", "*-*-*-*-*")); len(dirs) != 0 {
		t.Fatal("sessions left:", dirs)
	}
	if files, _ := filepath.Glob(filepath.Join(dir, "grok-home", "sessions", "*", grok.PromptHistoryFile)); len(files) != 0 {
		t.Fatal("prompt history left:", files)
	}
	if pending, _ := os.ReadDir(filepath.Join(tmp, "agent-mock", "pending")); len(pending) != 0 {
		t.Fatalf("pending records left: %v", pending)
	}
}
