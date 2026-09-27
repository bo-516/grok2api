package grok

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// PromptHistoryFile is the file grok 1.0.41 appends every submitted prompt to,
// verbatim, as {"timestamp","session_id","prompt","is_bash"} lines. There is one per
// session group. `grok sessions delete` leaves it in place, and grok's docs list no
// setting or variable that turns it off, so agent-mock removes it itself.
const PromptHistoryFile = "prompt_history.jsonl"

// maxGroupName is the longest encoded cwd grok uses as a group name as-is.
// A longer name becomes "<slug>-<hash>", and the cwd is written to a .cwd file inside.
const maxGroupName = 255

// GrokHome returns the data directory a grok child with environment env uses:
// GROK_HOME when set and non-empty, otherwise $HOME/.grok. env is a KEY=VALUE list;
// as with exec.Cmd.Env, the last entry for a key wins. Without HOME in env the
// current user's home directory is used. An empty result means no home was found
// and callers skip any cleanup that needs it.
func GrokHome(env []string) string {
	var grokHome, home string
	for _, kv := range env {
		key, val, _ := strings.Cut(kv, "=")
		switch key {
		case "GROK_HOME":
			grokHome = val
		case "HOME":
			home = val
		}
	}
	if grokHome != "" {
		return grokHome
	}
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".grok")
}

// EncodeCwd returns grok's session-group name for an absolute cwd with symlinks
// already resolved. Every byte outside A-Z, a-z, 0-9 and "-._~" becomes %XX in
// upper-case hex, so "/" is "%2F". This matches the directories grok 1.0.41 creates,
// including spaces, reserved characters, and non-ASCII bytes. Names over 255 bytes
// are not used by grok; see SessionGroup.
func EncodeCwd(cwd string) string {
	const digits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(cwd); i++ {
		c := cwd[i]
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(digits[c>>4])
		b.WriteByte(digits[c&0x0f])
	}
	return b.String()
}

// SessionGroup returns the directory under home/sessions where grok keeps the
// sessions and prompt history of runs started with --cwd cwd. cwd must exist,
// because grok resolves symlinks first (macOS /var is /private/var) and so does this.
// For a path whose encoded name is over 255 bytes the group is found through the
// .cwd file grok writes into it. ok is false when home is empty, cwd cannot be
// resolved, or a long-path group does not exist yet; callers then have nothing to clean.
// The returned path is always a direct child of home/sessions.
func SessionGroup(home, cwd string) (string, bool) {
	if home == "" {
		return "", false
	}
	real, err := filepath.EvalSymlinks(cwd)
	if err != nil || !filepath.IsAbs(real) {
		return "", false
	}
	sessions := filepath.Join(home, "sessions")
	if name := EncodeCwd(real); len(name) <= maxGroupName {
		return filepath.Join(sessions, name), true
	}
	entries, err := os.ReadDir(sessions)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(sessions, e.Name(), ".cwd"))
		if err == nil && strings.TrimRight(string(b), "\n") == real {
			return filepath.Join(sessions, e.Name()), true
		}
	}
	return "", false
}

// RemovePromptHistory unlinks group/prompt_history.jsonl. A missing file is not an
// error. Unlinking is safe while grok runs append to the file: a writer that already
// has it open writes into the unlinked copy, which disappears, and a later writer
// creates a new file that the next removal takes away. Truncating instead could leave
// a gap of zero bytes under a writer that seeks before writing.
// The returned error is only for logging; it never fails a request.
func RemovePromptHistory(group string) error {
	err := os.Remove(filepath.Join(group, PromptHistoryFile))
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
