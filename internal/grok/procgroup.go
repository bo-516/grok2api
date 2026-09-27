package grok

import (
	"context"
	"fmt"
	"syscall"
	"time"
)

// killGrace is the delay between SIGTERM and SIGKILL of a grok process group.
const killGrace = 5 * time.Second

// Close signals every grok process group this runner still tracks.
// SIGTERM is followed by SIGKILL after killGrace. A second call is a no-op.
// Handlers blocked in Run unblock once the child dies, so the HTTP server can
// finish Shutdown inside the 10s SIGINT budget.
func (r *Runner) Close() {
	r.mu.Lock()
	pids := make([]int, 0, len(r.active))
	for pid := range r.active {
		pids = append(pids, pid)
	}
	r.mu.Unlock()
	for _, pid := range pids {
		_ = signalGroup(pid, syscall.SIGTERM)
	}
	if len(pids) == 0 {
		return
	}
	time.Sleep(killGrace)
	for _, pid := range pids {
		_ = signalGroup(pid, syscall.SIGKILL)
	}
}

// track records a live process group leader.
func (r *Runner) track(pid int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active == nil {
		r.active = map[int]struct{}{}
	}
	r.active[pid] = struct{}{}
}

// untrack forgets a process group after Wait returns.
func (r *Runner) untrack(pid int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.active, pid)
}

// killOnCancel SIGTERMs pid when ctx ends, then SIGKILLs after killGrace.
// waitDone closed means the process already exited, so a recycled pid is not signaled.
func (r *Runner) killOnCancel(ctx context.Context, pid int, waitDone chan struct{}) {
	select {
	case <-ctx.Done():
		_ = signalGroup(pid, syscall.SIGTERM)
		select {
		case <-waitDone:
		case <-time.After(killGrace):
			_ = signalGroup(pid, syscall.SIGKILL)
		}
	case <-waitDone:
	}
}

// signalGroup delivers sig to the process group. A dead group returns an error that callers ignore.
func signalGroup(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return fmt.Errorf("bad pid")
	}
	err := syscall.Kill(-pid, sig)
	if err != nil {
		_ = syscall.Kill(pid, sig)
	}
	return err
}
