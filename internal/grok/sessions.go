package grok

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// deleteTimeout bounds one `grok sessions delete`. grok 1.0.41 needs about 1.3 s.
// Past this the child is killed and the session counts as not deleted.
const deleteTimeout = 15 * time.Second

// DeleteSession runs `grok sessions delete id` with stdin closed and waits for it.
// grok 1.0.41 exits 0 both for "Deleted session <id>" and for
// "No session found with id <id>.", so the output decides the result: either line
// means the session is gone and nil is returned. A start failure, a timeout, or any
// other output is an error, and the session may still be on disk.
// The child runs in its own process group, so a Ctrl-C in agent-mock's terminal
// (SIGINT to the foreground group) cannot kill a delete half way.
// bin is the grok executable and must not be empty. id must be a session UUID.
// parent is the environment before ChildEnv strips XAI_API_KEY.
func DeleteSession(bin, id string, parent []string) error {
	if bin == "" || !validSessionID(id) {
		return fmt.Errorf("grok sessions delete: bad executable %q or session id %q", bin, id)
	}
	ctx, cancel := context.WithTimeout(context.Background(), deleteTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "sessions", "delete", id)
	cmd.Env = ChildEnv(parent)
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// A grandchild that keeps the output pipe open must not hold the delete past exit.
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if strings.Contains(text, "Deleted session "+id) || strings.Contains(text, "No session found with id "+id) {
		return nil
	}
	if err == nil {
		err = errors.New("unexpected output")
	}
	if len(text) > 200 {
		text = text[:200]
	}
	return fmt.Errorf("grok sessions delete %s: %w: %q", id, err, text)
}

// NewSessionID returns a random UUIDv7 (RFC 9562) for grok's --session-id.
// The first 48 bits are the Unix time in milliseconds, so the ids sort by creation
// time like the ones grok generates itself. crypto/rand.Read cannot fail since
// Go 1.24 (it aborts the program instead), so there is no error to return.
func NewSessionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	ms := uint64(time.Now().UnixMilli())
	for i := 0; i < 6; i++ {
		b[i] = byte(ms >> (40 - 8*i))
	}
	b[6] = b[6]&0x0f | 0x70
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// validSessionID reports whether id has the 8-4-4-4-12 hex shape of a grok session id.
// Only such ids are passed to `grok sessions delete` or used as pending-record file
// names, so a stray value from grok's output (empty, "probe", text starting with "-",
// or a path) never becomes an argv flag or escapes the pending directory.
func validSessionID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
				return false
			}
		}
	}
	return true
}
