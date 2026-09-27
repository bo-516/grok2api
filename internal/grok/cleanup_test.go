package grok

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestRunCleansUpSessionAndHistory runs one prompt and checks that nothing of it is
// left in GROK_HOME: the session grok echoed from --session-id is deleted, the
// prompt_history.jsonl line is gone, and the pending record is dropped.
func TestRunCleansUpSessionAndHistory(t *testing.T) {
	r, dir, log := testRunner(t, false)
	final, err := r.Run(t.Context(), RunSpec{Prompt: "a private caller prompt"}, Events{})
	if err != nil {
		t.Fatal(err)
	}
	if !validSessionID(final.SessionID) {
		t.Fatalf("session id %q", final.SessionID)
	}
	waitDeletes(t, r)
	if got := deleted(t, dir); !slices.Equal(got, []string{final.SessionID}) {
		t.Fatalf("deleted %v, want [%s]", got, final.SessionID)
	}
	if dirs := sessionDirs(t, dir); len(dirs) != 0 {
		t.Fatal("sessions left:", dirs)
	}
	if files := historyFiles(t, dir); len(files) != 0 {
		t.Fatal("prompt history left:", files)
	}
	if ids := pendingIDs(t, r); len(ids) != 0 {
		t.Fatal("pending records left:", ids)
	}
	if log.String() != "" {
		t.Fatal(log.String())
	}
}

// TestRunKeepSessions leaves the session and the prompt history, deletes nothing,
// and writes no pending record.
func TestRunKeepSessions(t *testing.T) {
	r, dir, _ := testRunner(t, true)
	final, err := r.Run(t.Context(), RunSpec{Prompt: "keep me"}, Events{})
	if err != nil {
		t.Fatal(err)
	}
	waitDeletes(t, r)
	dirs := sessionDirs(t, dir)
	if len(dirs) != 1 || filepath.Base(dirs[0]) != final.SessionID {
		t.Fatalf("sessions %v, want %s", dirs, final.SessionID)
	}
	files := historyFiles(t, dir)
	if len(files) != 1 {
		t.Fatal("prompt history:", files)
	}
	if b, _ := os.ReadFile(files[0]); !strings.Contains(string(b), "keep me") {
		t.Fatal(string(b))
	}
	if got := deleted(t, dir); len(got) != 0 {
		t.Fatal("deleted", got)
	}
	if ids := pendingIDs(t, r); len(ids) != 0 {
		t.Fatal("pending records:", ids)
	}
}

// TestRunDeletesSessionsGrokNeverNamed covers runs where agent-mock never learns the
// session id from grok's stdout. Before --session-id these sessions stayed on disk:
// grok exiting before system/init, and a result record with no init (unsafe_grok_toolset).
func TestRunDeletesSessionsGrokNeverNamed(t *testing.T) {
	for _, scenario := range []string{"early-exit", "no-init"} {
		t.Run(scenario, func(t *testing.T) {
			r, dir, _ := testRunner(t, false, "FAKEGROK_SCENARIO="+scenario)
			id := NewSessionID()
			final, err := r.Run(t.Context(), RunSpec{Prompt: "x", SessionID: id}, Events{})
			if err == nil {
				t.Fatal("want an error")
			}
			if final.SessionID != id {
				t.Fatalf("final session %q, want the assigned %s", final.SessionID, id)
			}
			waitDeletes(t, r)
			if got := deleted(t, dir); !slices.Contains(got, id) {
				t.Fatalf("deleted %v, want %s", got, id)
			}
			if dirs := sessionDirs(t, dir); len(dirs) != 0 {
				t.Fatal("sessions left:", dirs)
			}
			if files := historyFiles(t, dir); len(files) != 0 {
				t.Fatal("prompt history left:", files)
			}
		})
	}
}

// TestSweep deletes sessions whose pending record belongs to a dead process, leaves
// records of live processes (another agent-mock, or this one) alone, and removes the
// prompt history. With KeepSessions it does nothing.
func TestSweep(t *testing.T) {
	r, dir, _ := testRunner(t, true)
	ids := make([]string, 3)
	for i := range ids {
		final, err := r.Run(t.Context(), RunSpec{Prompt: "left behind " + strconv.Itoa(i)}, Events{})
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = final.SessionID
	}
	writePending(t, r, ids[0], deadPID(t), "")
	writePending(t, r, ids[1], os.Getppid(), "")
	writePending(t, r, ids[2], os.Getpid(), "")

	r.Sweep()
	waitDeletes(t, r)
	if n := len(sessionDirs(t, dir)); n != 3 || len(historyFiles(t, dir)) != 1 {
		t.Fatalf("KeepSessions sweep touched the store: %d sessions left", n)
	}

	r.KeepSessions = false
	r.Sweep()
	waitDeletes(t, r)
	if got := deleted(t, dir); !slices.Equal(got, ids[:1]) {
		t.Fatalf("deleted %v, want only the dead owner's %s", got, ids[0])
	}
	var left []string
	for _, d := range sessionDirs(t, dir) {
		left = append(left, filepath.Base(d))
	}
	slices.Sort(left)
	want := slices.Clone(ids[1:])
	slices.Sort(want)
	if !slices.Equal(left, want) {
		t.Fatalf("sessions left %v, want %v", left, want)
	}
	if pend := pendingIDs(t, r); len(pend) != 2 || slices.Contains(pend, ids[0]) {
		t.Fatal("pending records:", pend)
	}
	if files := historyFiles(t, dir); len(files) != 0 {
		t.Fatal("prompt history left:", files)
	}
}

// TestRunOwnCwdHistory covers runs with their own --cwd (media runs use mcwd): grok
// keeps their prompts in that cwd's group, which cleanup and Sweep must scrub too.
func TestRunOwnCwdHistory(t *testing.T) {
	r, dir, _ := testRunner(t, false)
	mcwd := filepath.Join(r.Root, "mcwd")
	final, err := r.Run(t.Context(), RunSpec{Prompt: "x", Cwd: mcwd}, Events{})
	if err != nil {
		t.Fatal(err)
	}
	waitDeletes(t, r)
	if len(sessionDirs(t, dir)) != 0 || len(historyFiles(t, dir)) != 0 {
		t.Fatalf("run in %s left sessions %v or history %v", mcwd, sessionDirs(t, dir), historyFiles(t, dir))
	}

	// A crashed owner: the session and its history stay until the next Sweep.
	r.KeepSessions = true
	final, err = r.Run(t.Context(), RunSpec{Prompt: "x", Cwd: mcwd}, Events{})
	if err != nil {
		t.Fatal(err)
	}
	r.KeepSessions = false
	writePending(t, r, final.SessionID, deadPID(t), mcwd)
	r.Sweep()
	waitDeletes(t, r)
	if len(sessionDirs(t, dir)) != 0 || len(historyFiles(t, dir)) != 0 {
		t.Fatalf("Sweep left sessions %v or history %v", sessionDirs(t, dir), historyFiles(t, dir))
	}
}

// TestDeleteFailureIsRetried keeps the pending record and logs one line when grok
// fails to delete, and a later Sweep (after this process is gone, simulated by a dead
// owner pid) deletes the session.
func TestDeleteFailureIsRetried(t *testing.T) {
	r, dir, log := testRunner(t, false, "FAKEGROK_DELETE_FAIL=1")
	final, err := r.Run(t.Context(), RunSpec{Prompt: "x"}, Events{})
	if err != nil {
		t.Fatal(err)
	}
	waitDeletes(t, r)
	if pend := pendingIDs(t, r); !slices.Equal(pend, []string{final.SessionID}) {
		t.Fatalf("pending %v, want %s", pend, final.SessionID)
	}
	if !strings.Contains(log.String(), final.SessionID) || !strings.Contains(log.String(), "next start retries") {
		t.Fatal(log.String())
	}
	if len(sessionDirs(t, dir)) != 1 {
		t.Fatal("the failed delete must leave the session for the retry")
	}

	// The next start: same Root and GROK_HOME, grok deletes again, the old owner is gone.
	env := []string{"PATH=" + os.Getenv("PATH"), "GROK_HOME=" + filepath.Join(dir, "grok-home")}
	retry := &Runner{Bin: fakeBin, Root: r.Root, Environ: func() []string { return env }}
	writePending(t, retry, final.SessionID, deadPID(t), "")
	retry.Sweep()
	waitDeletes(t, retry)
	if len(sessionDirs(t, dir)) != 0 || len(pendingIDs(t, retry)) != 0 {
		t.Fatal("the retry did not delete the session")
	}
}

// TestWaitStopsAtContext returns false while a delete is still running and true
// once it finished.
func TestWaitStopsAtContext(t *testing.T) {
	r, _, _ := testRunner(t, false, "FAKEGROK_DELETE_DELAY=1s")
	if _, err := r.Run(t.Context(), RunSpec{Prompt: "x"}, Events{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if r.Wait(ctx) {
		t.Fatal("Wait returned true while the delete was sleeping")
	}
	start := time.Now()
	waitDeletes(t, r)
	if time.Since(start) > 3*time.Second {
		t.Fatal("Wait took too long:", time.Since(start))
	}
}

// writePending stores a pending record for id owned by pid, in markPending's format.
// cwd is the run's --cwd; empty writes the pid-only form, which means the chat cwd.
func writePending(t *testing.T, r *Runner, id string, pid int, cwd string) {
	t.Helper()
	if err := os.MkdirAll(r.pendingDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	body := strconv.Itoa(pid)
	if cwd != "" {
		body += "\n" + cwd + "\n"
	}
	if err := os.WriteFile(filepath.Join(r.pendingDir(), id), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// deadPID returns the pid of a child that already exited.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}
