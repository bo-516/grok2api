package grok

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"syscall"
)

// CheckToolset compares an init tools array with the run allowlist.
// extra is tools grok offered that are not in allow, in the order grok listed them.
// missing is allow entries grok did not offer, in allow order.
// raw must be a JSON array of strings. Null, missing, or any other shape returns
// an error: a chat run treats that as an unsafe toolset, because it is not proof
// of tools:[]. allow nil or empty is the chat allowlist, so only [] passes.
// Callers kill the process when extra is non-empty, and also when missing is
// non-empty unless the run is a startup probe (StopAfterInit).
func CheckToolset(raw json.RawMessage, allow []string) (extra, missing []string, err error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil, errToolsShape
	}
	var tools []string
	if json.Unmarshal(trimmed, &tools) != nil {
		return nil, nil, errToolsShape
	}
	allowed := make(map[string]bool, len(allow))
	for _, name := range allow {
		allowed[name] = true
	}
	seen := make(map[string]bool, len(tools))
	for _, name := range tools {
		if seen[name] {
			continue
		}
		seen[name] = true
		if !allowed[name] {
			extra = append(extra, name)
		}
	}
	for _, name := range allow {
		if !seen[name] {
			missing = append(missing, name)
		}
	}
	return extra, missing, nil
}

// errToolsShape is CheckToolset's error for a tools field that is not a string array.
// The text is not shown to HTTP clients; the runner maps it to unsafe_grok_toolset.
var errToolsShape = errString("grok init tools field is not a JSON array of strings")

// errString is a tiny error type so runner_util does not import fmt or errors
// only for this one sentinel. Error returns the text passed to errString.
type errString string

// Error returns the sentinel text.
func (e errString) Error() string { return string(e) }

// stripSecretEnv drops XAI_API_KEY from extra so a media ExtraEnv cannot undo ChildEnv.
// Other keys are kept in order. A nil or empty extra returns nil.
func stripSecretEnv(extra []string) []string {
	if len(extra) == 0 {
		return nil
	}
	out := make([]string, 0, len(extra))
	for _, kv := range extra {
		key, _, _ := strings.Cut(kv, "=")
		if key == "XAI_API_KEY" {
			continue
		}
		out = append(out, kv)
	}
	return out
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

// wrapTool invokes mark, then fn, and SIGKILLs pid when fn returns an error.
// A nil fn is a no-op so chat runs, which do not set tool callbacks, stay unchanged.
// ErrStop is an error here: the caller kills the group and treats the run as success.
func wrapTool[T any](fn func(T) error, mark func(), pid int) func(T) error {
	return func(v T) error {
		mark()
		if fn == nil {
			return nil
		}
		err := fn(v)
		if err != nil {
			_ = signalGroup(pid, syscall.SIGKILL)
		}
		return err
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
