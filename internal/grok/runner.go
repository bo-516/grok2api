package grok

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// initWait is how long a streaming run may go without any JSON record.
// Past this the process group is killed and the client gets grok_failed.
const initWait = 20 * time.Second

// killGrace is the delay between SIGTERM and SIGKILL of a grok process group.
const killGrace = 5 * time.Second

// stderrLimit is how much stderr is kept for error messages.
const stderrLimit = 4 * 1024

// Runner owns prompt files, the fixed empty cwd, and in-flight grok process groups.
// Close SIGTERMs every group so a SIGINT can finish within 10 seconds.
type Runner struct {
	// Bin is the grok executable. Empty means "grok" on PATH.
	Bin string
	// Root is the parent of cwd/ and prompts/. Empty uses os.TempDir()/agent-mock.
	Root string
	// KeepSessions skips `grok sessions delete` after a run.
	KeepSessions bool
	// Environ, when set, supplies the parent environment. Nil uses os.Environ.
	// Tests pass a slice that includes XAI_API_KEY and FAKEGROK_* variables.
	Environ func() []string

	mu     sync.Mutex
	active map[int]struct{}
}

// Run executes one restricted grok process.
// ev.OnMessageStart and ev.OnText run while stdout is still open, so the server
// can flush SSE before the final result. The prompt file is removed before Run returns.
// A cancelled ctx or a deadline SIGTERMs the process group and SIGKILLs it after 5s.
// DeadlineExceeded becomes CodeTimeout. A bare cancel is returned as ctx.Err()
// so a disconnected client is not reported as a timeout.
func (r *Runner) Run(ctx context.Context, spec RunSpec, ev Events) (Final, error) {
	bin := r.Bin
	if bin == "" {
		bin = "grok"
	}
	cwd, err := r.cwd()
	if err != nil {
		return Final{}, Failed("", err)
	}
	promptPath, err := r.writePrompt(spec.Prompt)
	if err != nil {
		return Final{}, Failed("", err)
	}
	defer os.Remove(promptPath)

	args := CommandArgs(promptPath, cwd, spec)
	cmd := exec.Command(bin, args...)
	cmd.Dir = cwd
	parent := os.Environ()
	if r.Environ != nil {
		parent = r.Environ()
	}
	cmd.Env = ChildEnv(parent)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Final{}, Failed("", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return Final{}, Failed("", err)
	}
	if err := cmd.Start(); err != nil {
		if errors.Is(err, exec.ErrNotFound) || os.IsNotExist(err) {
			return Final{}, &Error{Code: CodeNotFound, Message: "grok executable not found (" + bin + "). Install Grok Build and run `grok login`.", Err: err}
		}
		return Final{}, &Error{Code: CodeNotFound, Message: "could not start grok (" + bin + "): " + err.Error(), Err: err}
	}
	pid := cmd.Process.Pid
	r.track(pid)
	defer r.untrack(pid)

	tail := &tailBuf{max: stderrLimit}
	var stderrWG sync.WaitGroup
	stderrWG.Add(1)
	go func() {
		defer stderrWG.Done()
		_, _ = io.Copy(tail, stderr)
	}()

	waitDone := make(chan struct{})
	saw := make(chan struct{})
	var once sync.Once
	mark := func() { once.Do(func() { close(saw) }) }
	go r.killOnCancel(ctx, pid, waitDone)
	initTimedOut := make(chan struct{})
	go func() {
		timer := time.NewTimer(initWait)
		defer timer.Stop()
		select {
		case <-saw:
		case <-waitDone:
		case <-timer.C:
			close(initTimedOut)
			// No init line means the process is not a usable streaming run. Kill it
			// immediately so the 502 is not delayed by the cancel grace period.
			_ = signalGroup(pid, syscall.SIGKILL)
		}
	}()

	userInit := ev.OnInit
	ev.OnInit = func(info Init) error {
		mark()
		if !ToolsEmpty(info.ToolsRaw) {
			_ = signalGroup(pid, syscall.SIGKILL)
			shown := string(bytes.TrimSpace(info.ToolsRaw))
			if shown == "" {
				shown = "missing"
			}
			return &Error{Code: CodeUnsafe, Message: "grok toolset was " + shown + ", want []. The process was killed.", SessionID: info.SessionID}
		}
		if userInit != nil {
			if err := userInit(info); err != nil {
				return err
			}
		}
		if spec.StopAfterInit {
			_ = signalGroup(pid, syscall.SIGKILL)
			return errStop
		}
		return nil
	}
	ev.OnMessageStart = wrapMark(ev.OnMessageStart, mark)
	ev.OnText = wrapText(ev.OnText, mark)

	final, decErr := Decode(stdout, ev)
	mark()
	// A missing or non-empty toolset must not leave grok running. OnInit kills
	// when tools is not []; a streaming record with no init returns the same code.
	if ge, ok := AsError(decErr); ok && ge.Code == CodeUnsafe {
		_ = signalGroup(pid, syscall.SIGKILL)
	}
	// Drain leftover stdout so a child blocked on a full pipe can exit before Wait.
	go func() { _, _ = io.Copy(io.Discard, stdout) }()
	waitErr := cmd.Wait()
	close(waitDone)
	stderrWG.Wait()
	stderrText := tail.String()

	if errors.Is(decErr, errStop) {
		r.cleanup(final.SessionID)
		return final, nil
	}
	if ge, ok := AsError(decErr); ok && ge.Code == CodeUnsafe {
		r.cleanup(ge.SessionID)
		return final, ge
	}
	select {
	case <-initTimedOut:
		if !finalOK(final, decErr) {
			ge := Failed(stderrText, decErr)
			ge.SessionID = final.SessionID
			r.cleanup(final.SessionID)
			return final, ge
		}
	default:
	}
	if ctx.Err() != nil && !finalOK(final, decErr) {
		r.cleanup(final.SessionID)
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return final, &Error{Code: CodeTimeout, Message: "grok run exceeded the request timeout. Retry or raise -request-timeout.", SessionID: final.SessionID, Err: ctx.Err()}
		}
		return final, ctx.Err()
	}
	if ge, ok := AsError(decErr); ok {
		r.cleanup(firstID(ge.SessionID, final.SessionID))
		return final, ge
	}
	// Any decode error is a failed run, even when some text deltas already arrived.
	// A truncated or non-JSON stdout is grok_bad_output, not a 200 completion.
	if decErr != nil {
		ge := &Error{
			Code:      CodeBadOutput,
			Message:   "grok output could not be parsed",
			SessionID: final.SessionID,
			Err:       decErr,
		}
		if stderrText != "" {
			ge.Message = ge.Message + ". stderr: " + strings.TrimSpace(stderrText)
		}
		r.cleanup(final.SessionID)
		return final, ge
	}
	if waitErr != nil && decErr == nil && final.Text == "" && len(final.Structured) == 0 {
		ge := Classify("", stderrText)
		ge.SessionID = final.SessionID
		if ge.Code == CodeFailed && stderrText == "" {
			ge = Failed("", waitErr)
			ge.SessionID = final.SessionID
		}
		r.cleanup(final.SessionID)
		return final, ge
	}
	r.cleanup(final.SessionID)
	return final, nil
}

// finalOK reports whether Decode produced a usable success.
// An error result is not ok. errStop is handled by the caller before this.
func finalOK(f Final, decErr error) bool {
	if decErr != nil {
		return false
	}
	return f.Text != "" || len(f.Structured) != 0 || f.SessionID != "" && f.StopReason != ""
}

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

// cwd returns the shared empty working directory, creating it if needed.
// Every run uses this same directory so grok cannot see a project checkout.
func (r *Runner) cwd() (string, error) {
	dir := filepath.Join(r.root(), "cwd")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// writePrompt creates a 0600 prompt file and returns its path.
// The file is the only place the user message is stored; it is not an argv argument.
func (r *Runner) writePrompt(body string) (string, error) {
	dir := filepath.Join(r.root(), "prompts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	path := filepath.Join(dir, hex.EncodeToString(buf[:])+".txt")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	_, werr := io.WriteString(f, body)
	cerr := f.Close()
	if werr != nil {
		os.Remove(path)
		return "", werr
	}
	if cerr != nil {
		os.Remove(path)
		return "", cerr
	}
	return path, nil
}

// root is the agent-mock temp directory.
func (r *Runner) root() string {
	if r.Root != "" {
		return r.Root
	}
	return filepath.Join(os.TempDir(), "agent-mock")
}

// cleanup deletes the grok session unless KeepSessions is set or id is empty.
// Deletion is best-effort and does not block the HTTP response.
func (r *Runner) cleanup(id string) {
	if r.KeepSessions || id == "" || id == "probe" {
		return
	}
	bin := r.Bin
	if bin == "" {
		bin = "grok"
	}
	parent := os.Environ()
	if r.Environ != nil {
		parent = append([]string(nil), r.Environ()...)
	}
	go DeleteSession(bin, id, parent)
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

// ToolsEmpty reports whether init.tools is a JSON empty array.
// Null, missing, or a non-empty array is not empty: the run must be killed.
func ToolsEmpty(raw []byte) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("[]"))
}

// wrapMark invokes mark before the message-start callback so the init timer stops
// even when the first record is a message rather than system/init.
func wrapMark(fn func(string, string) error, mark func()) func(string, string) error {
	return func(sessionID, model string) error {
		mark()
		if fn == nil {
			return nil
		}
		return fn(sessionID, model)
	}
}

// wrapText invokes mark before forwarding a delta. A text-only JSON result has no deltas.
func wrapText(fn func(string) error, mark func()) func(string) error {
	return func(delta string) error {
		mark()
		if fn == nil {
			return nil
		}
		return fn(delta)
	}
}

// firstID returns the first non-empty session id.
func firstID(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// tailBuf keeps the last max bytes written to it.
type tailBuf struct {
	mu  sync.Mutex
	buf []byte
	max int
}

// Write appends p and drops bytes older than max. It never fails.
func (t *tailBuf) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

// String returns the retained stderr tail.
func (t *tailBuf) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
