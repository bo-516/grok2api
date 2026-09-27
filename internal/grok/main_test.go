package grok

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeBin is the fakegrok binary TestMain builds for the runner tests.
var fakeBin string

// TestMain builds internal/testutil/fakegrok once, so runner tests drive a real child
// process that writes sessions and prompt history the way grok 1.0.41 does.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "agent-mock-grok-test")
	if err != nil {
		panic(err)
	}
	fakeBin = filepath.Join(dir, "fakegrok")
	cmd := exec.Command("go", "build", "-o", fakeBin, "./internal/testutil/fakegrok")
	cmd.Dir = filepath.Join("..", "..")
	if out, err := cmd.CombinedOutput(); err != nil {
		panic(string(out) + "\n" + err.Error())
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// syncBuf is a Log writer that tests read while deletes may still write to it.
type syncBuf struct {
	// mu guards b.
	mu sync.Mutex
	// b holds every line written so far.
	b bytes.Buffer
}

// Write appends p under the lock.
func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

// String returns everything written so far.
func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// testRunner returns a Runner on fakegrok whose Root, HOME, GROK_HOME, and delete log
// all live in the returned t.TempDir(), so neither the fake nor the runner's cleanup
// can reach the developer's ~/.grok. keep sets KeepSessions. extra adds FAKEGROK_*
// settings; FAKEGROK_SCENARIO defaults to the text fixture.
func testRunner(t *testing.T, keep bool, extra ...string) (*Runner, string, *syncBuf) {
	t.Helper()
	dir := t.TempDir()
	fixtures, err := filepath.Abs(filepath.Join("..", "..", "testdata", "grok"))
	if err != nil {
		t.Fatal(err)
	}
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + filepath.Join(dir, "home"),
		"GROK_HOME=" + filepath.Join(dir, "grok-home"),
		"FAKEGROK_FIXTURE_DIR=" + fixtures,
		"FAKEGROK_DELETE_LOG=" + filepath.Join(dir, "deletes"),
		"FAKEGROK_SCENARIO=text",
	}
	env = append(env, extra...)
	log := &syncBuf{}
	r := &Runner{
		Bin:          fakeBin,
		Root:         filepath.Join(dir, "root"),
		KeepSessions: keep,
		Log:          log,
		Environ:      func() []string { return env },
	}
	// Deletes still running write into dir; finish them before t.TempDir removes it.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		r.Wait(ctx)
	})
	return r, dir, log
}

// waitDeletes blocks until the runner's background deletes finish.
func waitDeletes(t *testing.T, r *Runner) {
	t.Helper()
	if !r.Wait(t.Context()) {
		t.Fatal("session deletes did not finish")
	}
}

// sessionDirs lists the session directories left under dir's GROK_HOME, in any group.
func sessionDirs(t *testing.T, dir string) []string {
	t.Helper()
	dirs, err := filepath.Glob(filepath.Join(dir, "grok-home", "sessions", "*", "*-*-*-*-*"))
	if err != nil {
		t.Fatal(err)
	}
	return dirs
}

// historyFiles lists the prompt_history.jsonl files left under dir's GROK_HOME.
func historyFiles(t *testing.T, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "grok-home", "sessions", "*", PromptHistoryFile))
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// deleted returns the ids fakegrok was asked to delete, in order.
func deleted(t *testing.T, dir string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "deletes"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(b))
}

// pendingIDs lists the pending-delete records under the runner's Root.
func pendingIDs(t *testing.T, r *Runner) []string {
	t.Helper()
	entries, err := os.ReadDir(r.pendingDir())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, e := range entries {
		ids = append(ids, e.Name())
	}
	return ids
}
