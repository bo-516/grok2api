package grok

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// cleanup runs once per Run, after the grok process exited, so the session directory
// and the prompt-history line are complete. cwd is the --cwd the run used; its group's
// prompt_history.jsonl is removed. Then each distinct session id in ids is deleted in
// the background: the id agent-mock passed with --session-id and any different id grok
// reported. ids that are empty or not UUID-shaped are skipped. Nothing happens when
// KeepSessions is set. The deletes are counted so Wait can hold shutdown until they finish.
func (r *Runner) cleanup(cwd string, ids ...string) {
	if r.KeepSessions {
		return
	}
	r.scrubHistory(cwd)
	seen := map[string]bool{}
	for _, id := range ids {
		if !validSessionID(id) || seen[id] {
			continue
		}
		seen[id] = true
		r.deletes.add()
		go func() {
			defer r.deletes.done()
			r.deleteSession(id)
		}()
	}
}

// deleteSession deletes one session and waits for grok to confirm it is gone. On
// success the pending record for id is removed. On failure the record stays, one line
// goes to Log, and the next Sweep retries, so a failed delete is never silent.
func (r *Runner) deleteSession(id string) {
	if err := DeleteSession(r.bin(), id, r.environ()); err != nil {
		r.logf("agent-mock: grok session %s was not deleted (%v); the next start retries it", id, err)
		return
	}
	r.unmarkPending(id)
}

// scrubHistory removes prompt_history.jsonl from the session group of cwd, a directory
// agent-mock runs grok in; empty means the shared chat cwd. It is only called when
// KeepSessions is false. These groups belong to agent-mock: nothing else runs grok in
// its private directories, so every line there is a caller prompt. A failure is
// logged and does not affect the request.
func (r *Runner) scrubHistory(cwd string) {
	if cwd == "" {
		var err error
		if cwd, err = r.cwd(); err != nil {
			return
		}
	}
	group, ok := SessionGroup(GrokHome(r.environ()), cwd)
	if !ok {
		return
	}
	if err := RemovePromptHistory(group); err != nil {
		r.logf("agent-mock: could not remove grok prompt history: %v", err)
	}
}

// Sweep cleans up after earlier agent-mock processes and should run once at startup.
// It removes the shared chat cwd's prompt_history.jsonl right away. Then, for every
// pending record whose owner process no longer runs (an agent-mock that was killed, or
// whose shutdown ran out of time), it removes the prompt history of the cwd that run
// used and deletes the session in the background. Records of live processes,
// including this one, are left to their owner. With KeepSessions set, Sweep does
// nothing, so kept sessions and their history stay untouched.
func (r *Runner) Sweep() {
	if r.KeepSessions {
		return
	}
	r.scrubHistory("")
	dir := r.pendingDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var orphans []string
	scrubbed := map[string]bool{}
	for _, e := range entries {
		id := e.Name()
		if !validSessionID(id) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, id))
		if err != nil {
			continue
		}
		owner, cwd, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
		pid, _ := strconv.Atoi(strings.TrimSpace(owner))
		if ownerAlive(pid) {
			continue
		}
		orphans = append(orphans, id)
		if cwd = strings.TrimSpace(cwd); cwd != "" && !scrubbed[cwd] {
			scrubbed[cwd] = true
			r.scrubHistory(cwd)
		}
	}
	if len(orphans) == 0 {
		return
	}
	r.deletes.add()
	go func() {
		defer r.deletes.done()
		for _, id := range orphans {
			r.deleteSession(id)
		}
	}()
}

// Wait blocks until every background session delete started so far has finished, or
// until ctx ends. It returns true when all deletes finished. Deletes cut off by ctx
// keep their pending record and are retried by the next Sweep.
func (r *Runner) Wait(ctx context.Context) bool {
	select {
	case <-r.deletes.idle():
		return true
	case <-ctx.Done():
		return false
	}
}

// pendingDir is <root>/pending. A file there is named after a session id and holds
// the pid of the agent-mock process that still has to delete that session, then on
// a second line the --cwd of the run, so Sweep knows which prompt history to remove.
func (r *Runner) pendingDir() string {
	return filepath.Join(r.root(), "pending")
}

// markPending records id and the run's cwd before grok starts, so the session is
// deleted and its prompt history removed by a later Sweep even if this process dies
// before its own cleanup. The record is written under a temporary name and renamed,
// so Sweep never reads a partial one. An error means the run has no crash
// protection; the normal cleanup still happens.
func (r *Runner) markPending(id, cwd string) error {
	if !validSessionID(id) {
		return fmt.Errorf("bad session id %q", id)
	}
	dir := r.pendingDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+id+".tmp")
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(os.Getpid())+"\n"+cwd+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, id))
}

// unmarkPending drops the record for id. A missing record is fine.
func (r *Runner) unmarkPending(id string) {
	if validSessionID(id) {
		_ = os.Remove(filepath.Join(r.pendingDir(), id))
	}
}

// ownerAlive reports whether pid still runs or is this process. Sweep leaves such
// records to their owner. A pid of 0 or less (a garbled record) counts as gone.
// EPERM means the pid exists under another user, so it also counts as alive.
func ownerAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if pid == os.Getpid() {
		return true
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// logf writes one line to Log. A nil Log drops it. Lines from concurrent deletes
// do not interleave.
func (r *Runner) logf(format string, args ...any) {
	if r.Log == nil {
		return
	}
	r.logMu.Lock()
	defer r.logMu.Unlock()
	fmt.Fprintf(r.Log, format+"\n", args...)
}

// tracker counts background deletes. Unlike sync.WaitGroup it allows a new delete
// to start while Wait is blocked, which happens when shutdown runs out of time.
// The zero value is ready to use.
type tracker struct {
	// mu guards n and waiters.
	mu sync.Mutex
	// n is the number of deletes still running.
	n int
	// waiters are closed the next time n drops to zero.
	waiters []chan struct{}
}

// add counts one more running delete.
func (t *tracker) add() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.n++
}

// done marks one delete finished and wakes the waiters once none remain.
func (t *tracker) done() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.n--
	if t.n > 0 {
		return
	}
	for _, w := range t.waiters {
		close(w)
	}
	t.waiters = nil
}

// idle returns a channel that is closed once no delete is running. It is already
// closed when nothing runs at the time of the call.
func (t *tracker) idle() <-chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	ch := make(chan struct{})
	if t.n == 0 {
		close(ch)
		return ch
	}
	t.waiters = append(t.waiters, ch)
	return ch
}
