package grok

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
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

// stderrLimit is how much stderr is kept for error messages.
const stderrLimit = 4 * 1024

// Runner owns prompt files, the fixed empty cwd, in-flight grok process groups, and
// the cleanup of what grok leaves on disk: the session and the prompt-history line.
// Close SIGTERMs every group so a SIGINT can finish within 10 seconds; Wait then lets
// background session deletes finish. The zero value is ready to use.
type Runner struct {
	// Bin is the grok executable. Empty means "grok" on PATH.
	Bin string
	// Root is the parent of cwd/, prompts/, and pending/. Empty uses os.TempDir()/agent-mock.
	Root string
	// KeepSessions turns all cleanup off: no `grok sessions delete`, no pending
	// records, and grok's prompt_history.jsonl stays. False deletes each session
	// after its run and removes the prompt history.
	KeepSessions bool
	// Environ, when set, supplies the parent environment. Nil uses os.Environ.
	// Tests pass a slice that includes XAI_API_KEY, GROK_HOME, and FAKEGROK_* variables.
	// GROK_HOME and HOME in it also decide where prompt_history.jsonl is removed.
	Environ func() []string
	// Log receives one line per cleanup problem, such as a session grok did not
	// delete. Nil discards the lines. It is never given prompt text.
	Log io.Writer

	// mu guards active.
	mu sync.Mutex
	// active holds the pids of running grok process-group leaders.
	active map[int]struct{}
	// logMu keeps concurrent Log lines whole.
	logMu sync.Mutex
	// deletes counts background `grok sessions delete` calls for Wait.
	deletes tracker
}

// Run executes one restricted grok process.
// ev.OnMessageStart and ev.OnText run while stdout is still open, so the server
// can flush SSE before the final result. The prompt file is removed before Run returns.
// A cancelled ctx or a deadline SIGTERMs the process group and SIGKILLs it after 5s.
// DeadlineExceeded becomes CodeTimeout. A bare cancel is returned as ctx.Err()
// so a disconnected client is not reported as a timeout.
// An empty spec.SessionID gets a fresh UUID, so the session can be deleted even when
// grok dies before printing its id. Final.SessionID falls back to that id.
// Unless KeepSessions is set, the session is deleted in the background after grok
// exits and prompt_history.jsonl is removed; see cleanup.
func (r *Runner) Run(ctx context.Context, spec RunSpec, ev Events) (Final, error) {
	bin := r.bin()
	cwd, err := r.workDir(spec.Cwd)
	if err != nil {
		return Final{}, Failed("", err)
	}
	promptPath, err := r.writePrompt(spec.Prompt)
	if err != nil {
		return Final{}, Failed("", err)
	}
	defer os.Remove(promptPath)
	if spec.SessionID == "" {
		spec.SessionID = NewSessionID()
	}
	if !r.KeepSessions {
		if err := r.markPending(spec.SessionID, cwd); err != nil {
			r.logf("agent-mock: could not record pending grok session %s: %v", spec.SessionID, err)
		}
	}

	args := CommandArgs(promptPath, cwd, spec)
	cmd := exec.Command(bin, args...)
	cmd.Dir = cwd
	cmd.Env = append(ChildEnv(r.environ()), stripSecretEnv(spec.ExtraEnv)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		r.unmarkPending(spec.SessionID)
		return Final{}, Failed("", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		r.unmarkPending(spec.SessionID)
		return Final{}, Failed("", err)
	}
	if err := cmd.Start(); err != nil {
		// grok never ran, so there is no session to delete.
		r.unmarkPending(spec.SessionID)
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
		extra, missing, cerr := CheckToolset(info.ToolsRaw, spec.Tools)
		if userInit != nil && cerr == nil {
			if err := userInit(info); err != nil {
				_ = signalGroup(pid, syscall.SIGKILL)
				return err
			}
		}
		if cerr != nil || len(extra) > 0 {
			_ = signalGroup(pid, syscall.SIGKILL)
			shown := string(bytes.TrimSpace(info.ToolsRaw))
			if shown == "" {
				shown = "missing"
			}
			want := "[]"
			if len(spec.Tools) > 0 {
				b, _ := json.Marshal(spec.Tools)
				want = string(b)
			}
			return &Error{Code: CodeUnsafe, Message: "grok toolset was " + shown + ", want " + want + ". The process was killed.", SessionID: info.SessionID}
		}
		if len(missing) > 0 && !spec.StopAfterInit {
			_ = signalGroup(pid, syscall.SIGKILL)
			return &Error{Code: CodeMediaUnavailable, Message: "grok media toolset is missing " + strings.Join(missing, ",") + ". The process was killed.", SessionID: info.SessionID}
		}
		if spec.StopAfterInit {
			_ = signalGroup(pid, syscall.SIGKILL)
			return ErrStop
		}
		return nil
	}
	ev.OnMessageStart = wrapMark(ev.OnMessageStart, mark)
	ev.OnText = wrapText(ev.OnText, mark)
	ev.OnToolUse = wrapTool(ev.OnToolUse, mark, pid)
	ev.OnToolResult = wrapTool(ev.OnToolResult, mark, pid)

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

	// grok has exited, so its session directory and prompt-history line are final.
	// The assigned id also covers runs that died before grok printed any id.
	r.cleanup(cwd, spec.SessionID, final.SessionID, errSessionID(decErr))
	usable := finalOK(final, decErr)
	final.SessionID = firstID(final.SessionID, spec.SessionID)

	if errors.Is(decErr, ErrStop) {
		return final, nil
	}
	if ge, ok := AsError(decErr); ok && ge.Code == CodeUnsafe {
		ge.SessionID = firstID(ge.SessionID, final.SessionID)
		return final, ge
	}
	select {
	case <-initTimedOut:
		if !usable {
			ge := Failed(stderrText, decErr)
			ge.SessionID = final.SessionID
			return final, ge
		}
	default:
	}
	if ctx.Err() != nil && !usable {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return final, &Error{Code: CodeTimeout, Message: "grok run exceeded the request timeout. Retry or raise -request-timeout.", SessionID: final.SessionID, Err: ctx.Err()}
		}
		return final, ctx.Err()
	}
	if ge, ok := AsError(decErr); ok {
		ge.SessionID = firstID(ge.SessionID, final.SessionID)
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
		return final, ge
	}
	if waitErr != nil && decErr == nil && final.Text == "" && len(final.Structured) == 0 {
		ge := Classify("", stderrText)
		ge.SessionID = final.SessionID
		if ge.Code == CodeFailed && stderrText == "" {
			ge = Failed("", waitErr)
			ge.SessionID = final.SessionID
		}
		return final, ge
	}
	return final, nil
}

// errSessionID returns the session id a *Error carries, or "" for any other error.
// cleanup uses it so a session named only in an error record is deleted too.
func errSessionID(err error) string {
	if ge, ok := AsError(err); ok {
		return ge.SessionID
	}
	return ""
}

// finalOK reports whether Decode produced a usable success.
// An error result is not ok. ErrStop is handled by the caller before this.
func finalOK(f Final, decErr error) bool {
	if decErr != nil {
		return false
	}
	return f.Text != "" || len(f.Structured) != 0 || f.SessionID != "" && f.StopReason != ""
}

// workDir returns the run directory. An empty cwd uses the shared chat directory.
// A set cwd (media mcwd) is created at 0700 and used as-is so media runs do not
// share the chat directory. A mkdir failure is returned to the caller as grok_failed.
func (r *Runner) workDir(cwd string) (string, error) {
	if cwd == "" {
		return r.cwd()
	}
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		return "", err
	}
	return cwd, nil
}

// cwd returns the shared empty working directory, creating it if needed.
// Every chat run uses this same directory so grok cannot see a project checkout.
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

// RootDir is the parent of cwd/, prompts/, mcwd/, and the media store.
// An empty Runner.Root means os.TempDir()/agent-mock. Callers use it to place
// files the process must delete on shutdown. It does not create the directory.
func (r *Runner) RootDir() string { return r.root() }

// root is the agent-mock temp directory.
func (r *Runner) root() string {
	if r.Root != "" {
		return r.Root
	}
	return filepath.Join(os.TempDir(), "agent-mock")
}

// bin is the grok executable: Bin, or "grok" on PATH when Bin is empty.
func (r *Runner) bin() string {
	if r.Bin == "" {
		return "grok"
	}
	return r.Bin
}

// environ returns a copy of the parent environment for grok children: Environ()
// when set, otherwise os.Environ(). Callers pass it through ChildEnv before exec.
func (r *Runner) environ() []string {
	if r.Environ != nil {
		return append([]string(nil), r.Environ()...)
	}
	return os.Environ()
}
