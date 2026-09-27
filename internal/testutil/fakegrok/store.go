package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// uuidShape matches a session id. Deletes of anything else report "not found"
// without touching the disk, so a bad id can never reach os.RemoveAll.
var uuidShape = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// sessionField matches a non-empty session id field in recorded grok output, in the
// NDJSON (session_id) and single-object JSON (sessionId) spellings.
var sessionField = regexp.MustCompile(`("(?:session_id|sessionId)"\s*:\s*)"[^"]+"`)

// storeRoot is GROK_HOME/sessions, or "" when GROK_HOME is unset. The fake persists
// nothing without GROK_HOME, so a test can never write into the developer's ~/.grok.
func storeRoot() string {
	home := os.Getenv("GROK_HOME")
	if home == "" {
		return ""
	}
	return filepath.Join(home, "sessions")
}

// groupName is grok 1.0.41's session-group name for a symlink-free cwd: every byte
// except A-Z a-z 0-9 - . _ ~ percent-encoded in upper-case hex. It is written apart
// from the grok package on purpose, so the two encoders check each other in tests.
// QueryEscape already keeps exactly that set; only its "+" for a space differs.
func groupName(cwd string) string {
	return strings.ReplaceAll(url.QueryEscape(cwd), "+", "%20")
}

// persist does what grok 1.0.41 does on disk when a prompt is submitted: it appends
// {"timestamp","session_id","prompt","is_bash"} to <group>/prompt_history.jsonl and
// creates <group>/<id>/summary.json. It runs before any stdout, like grok, so a run
// that dies before system/init still leaves both behind. It does nothing without
// GROK_HOME, --cwd, or --session-id.
func persist(args []string, body, id string) {
	root := storeRoot()
	cwd := flagValue(args, "--cwd")
	if root == "" || cwd == "" || id == "" {
		return
	}
	real, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return
	}
	group := filepath.Join(root, groupName(real))
	if err := os.MkdirAll(filepath.Join(group, id), 0o700); err != nil {
		return
	}
	summary := fmt.Sprintf(`{"info":{"id":%q,"cwd":%q}}`, id, real)
	_ = os.WriteFile(filepath.Join(group, id, "summary.json"), []byte(summary), 0o644)
	line, err := json.Marshal(map[string]any{
		"timestamp":  time.Now().UTC().Format(time.RFC3339Nano),
		"session_id": id,
		"prompt":     strings.TrimSuffix(body, "\n"),
		"is_bash":    false,
	})
	if err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(group, "prompt_history.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	_, _ = f.Write(append(line, '\n'))
	_ = f.Close()
}

// withSession rewrites every non-empty session id in recorded output to id, the
// way grok echoes a --session-id. An empty id returns b unchanged. Empty ids stay
// empty, as in a logged-out init.
func withSession(b []byte, id string) []byte {
	if id == "" {
		return b
	}
	return sessionField.ReplaceAll(b, []byte(`${1}"`+id+`"`))
}

// idOr returns the --session-id value, or legacy when the flag is absent.
func idOr(args []string, legacy string) string {
	if id := flagValue(args, "--session-id"); id != "" {
		return id
	}
	return legacy
}

// sessions implements `grok sessions delete <id>` like grok 1.0.41: it removes the
// session directory from whichever group holds it and prints "Deleted session <id>",
// or prints "No session found with id <id>."; both exit 0 and stdin is never read.
// FAKEGROK_DELETE_DELAY sleeps first, to test shutdown. FAKEGROK_DELETE_FAIL=1 prints
// an error and exits 1 without deleting. After a delete that did not fail, the id is
// appended to FAKEGROK_DELETE_LOG when that path is set.
func sessions(args []string) {
	if len(args) < 2 || args[0] != "delete" {
		fmt.Println("SESSION ID")
		return
	}
	id := args[1]
	if d, err := time.ParseDuration(os.Getenv("FAKEGROK_DELETE_DELAY")); err == nil {
		time.Sleep(d)
	}
	if os.Getenv("FAKEGROK_DELETE_FAIL") == "1" {
		fmt.Fprintln(os.Stderr, "Error: session store is locked")
		os.Exit(1)
	}
	if removeSession(id) {
		fmt.Printf("Deleted session %s\n", id)
	} else {
		fmt.Printf("No session found with id %s.\n", id)
	}
	if p := os.Getenv("FAKEGROK_DELETE_LOG"); p != "" {
		f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprintln(f, id)
			_ = f.Close()
		}
	}
}

// removeSession deletes <GROK_HOME>/sessions/<group>/<id> and reports whether one
// existed. The group's prompt_history.jsonl is left alone, as grok does.
func removeSession(id string) bool {
	root := storeRoot()
	if root == "" || !uuidShape.MatchString(id) {
		return false
	}
	dirs, _ := filepath.Glob(filepath.Join(root, "*", id))
	for _, dir := range dirs {
		_ = os.RemoveAll(dir)
	}
	return len(dirs) > 0
}
