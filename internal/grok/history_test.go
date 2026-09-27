package grok

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEncodeCwd locks the group names grok 1.0.41 created on 2026-09-26: symlinks
// already resolved, "/" as %2F, "-._~" kept, and every other byte (space, reserved
// characters, UTF-8) as upper-case %XX.
func TestEncodeCwd(t *testing.T) {
	for cwd, want := range map[string]string{
		"/private/var/folders/yd/p9y4mff90q3b_r81c07v5hcr0000gn/T/agent-mock/cwd": "%2Fprivate%2Fvar%2Ffolders%2Fyd%2Fp9y4mff90q3b_r81c07v5hcr0000gn%2FT%2Fagent-mock%2Fcwd",
		"/Users/dev/.grok/worktrees/demo/subagent-01a05136":                       "%2FUsers%2Fdev%2F.grok%2Fworktrees%2Fdemo%2Fsubagent-01a05136",
		"/private/tmp/exp2/odd dir+a~b.c_d@e(f)ü,g;h=i&j$k'l":                     "%2Fprivate%2Ftmp%2Fexp2%2Fodd%20dir%2Ba~b.c_d%40e%28f%29%C3%BC%2Cg%3Bh%3Di%26j%24k%27l",
	} {
		if got := EncodeCwd(cwd); got != want {
			t.Errorf("EncodeCwd(%q)\n got %s\nwant %s", cwd, got, want)
		}
	}
}

// TestSessionGroupResolvesSymlinks names the group after the resolved cwd, as grok
// does for agent-mock's $TMPDIR/agent-mock/cwd on macOS (/var is /private/var).
func TestSessionGroupResolvesSymlinks(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real", "cwd")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "real"), filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(base, "home")
	got, ok := SessionGroup(home, filepath.Join(base, "link", "cwd"))
	canon, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "sessions", EncodeCwd(canon)); !ok || got != want {
		t.Fatalf("got %s %v, want %s", got, ok, want)
	}
	if _, ok := SessionGroup("", real); ok {
		t.Fatal("an empty home must not resolve")
	}
	if _, ok := SessionGroup(home, filepath.Join(base, "missing")); ok {
		t.Fatal("a missing cwd must not resolve")
	}
}

// TestSessionGroupLongPath finds a group whose encoded name would exceed 255 bytes
// through the .cwd file grok writes into its "<leaf>-<hash>" directory.
func TestSessionGroupLongPath(t *testing.T) {
	base := t.TempDir()
	long := filepath.Join(base, strings.Repeat("long-segment/", 20), "leaf")
	if err := os.MkdirAll(long, 0o700); err != nil {
		t.Fatal(err)
	}
	canon, err := filepath.EvalSymlinks(long)
	if err != nil {
		t.Fatal(err)
	}
	if len(EncodeCwd(canon)) <= maxGroupName {
		t.Fatal("test path is not long enough")
	}
	home := filepath.Join(base, "home")
	if _, ok := SessionGroup(home, long); ok {
		t.Fatal("no group exists yet")
	}
	sessions := filepath.Join(home, "sessions")
	for name, cwd := range map[string]string{"leaf-0000000000000000": "/elsewhere/leaf", "leaf-57880989dafa0b2a": canon} {
		if err := os.MkdirAll(filepath.Join(sessions, name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sessions, name, ".cwd"), []byte(cwd), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, ok := SessionGroup(home, long)
	if want := filepath.Join(sessions, "leaf-57880989dafa0b2a"); !ok || got != want {
		t.Fatalf("got %s %v, want %s", got, ok, want)
	}
}

// TestGrokHome prefers a non-empty GROK_HOME, falls back to $HOME/.grok, and lets
// the last duplicate win like exec.Cmd.Env.
func TestGrokHome(t *testing.T) {
	for want, env := range map[string][]string{
		"/g":       {"HOME=/h", "GROK_HOME=/g"},
		"/h/.grok": {"HOME=/h", "GROK_HOME="},
		"/b":       {"GROK_HOME=/a", "HOME=/h", "GROK_HOME=/b"},
	} {
		if got := GrokHome(env); got != want {
			t.Errorf("GrokHome(%v) = %s, want %s", env, got, want)
		}
	}
}

// TestRemovePromptHistory removes only prompt_history.jsonl, accepts a missing file,
// and is safe while a grok writer still has the file open for appending: the writer
// keeps succeeding and nothing it writes afterwards reappears under the old name.
func TestRemovePromptHistory(t *testing.T) {
	group := t.TempDir()
	session := filepath.Join(group, NewSessionID())
	if err := os.MkdirAll(session, 0o700); err != nil {
		t.Fatal(err)
	}
	hist := filepath.Join(group, PromptHistoryFile)
	writer, err := os.OpenFile(hist, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.WriteString(`{"prompt":"first"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := RemovePromptHistory(group); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.WriteString(`{"prompt":"second"}` + "\n"); err != nil {
		t.Fatal("a writer with the file open must keep working:", err)
	}
	if _, err := os.Stat(hist); !os.IsNotExist(err) {
		t.Fatal("prompt history still reachable:", err)
	}
	if _, err := os.Stat(session); err != nil {
		t.Fatal("session directory must be left to grok sessions delete:", err)
	}
	if err := RemovePromptHistory(group); err != nil {
		t.Fatal("a missing file is not an error:", err)
	}
}
