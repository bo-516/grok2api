package grok

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// uuidV7 is the RFC 9562 shape NewSessionID must produce: version 7, variant 10.
var uuidV7 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// TestNewSessionID produces unique, time-ordered UUIDv7 ids that grok accepts as
// --session-id and that validSessionID accepts for deletion.
func TestNewSessionID(t *testing.T) {
	seen := map[string]bool{}
	prev := ""
	for i := 0; i < 1000; i++ {
		id := NewSessionID()
		if !uuidV7.MatchString(id) || !validSessionID(id) {
			t.Fatalf("bad id %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
		// The 48-bit millisecond prefix never goes backwards.
		if prefix := id[:13]; prefix < prev {
			t.Fatalf("time prefix went backwards: %s after %s", prefix, prev)
		} else {
			prev = prefix
		}
	}
}

// TestValidSessionID accepts grok's ids and rejects values that must never reach
// argv or a file name.
func TestValidSessionID(t *testing.T) {
	for _, id := range []string{"01a0d98a-4710-73d3-b739-562093fead03", "84D55CD4-2749-47AA-B896-EE421EACE189"} {
		if !validSessionID(id) {
			t.Errorf("rejected %q", id)
		}
	}
	for _, id := range []string{"", "probe", "abc-def", "--help", "../../../../etc/passwd", "01a0d98a-4710-73d3-b739-562093fead0g", "01a0d98a-4710-73d3-b739_562093fead03", "01a0d98a-4710-73d3-b739-562093fead033"} {
		if validSessionID(id) {
			t.Errorf("accepted %q", id)
		}
	}
}

// TestDeleteSession reads grok's output, because grok 1.0.41 exits 0 whether or not
// the session existed: "Deleted session" and "No session found" are both success,
// an error exit is a failure, and an invalid id never starts grok.
func TestDeleteSession(t *testing.T) {
	r, dir, _ := testRunner(t, true)
	final, err := r.Run(t.Context(), RunSpec{Prompt: "x"}, Events{})
	if err != nil {
		t.Fatal(err)
	}
	env := r.environ()
	if err := DeleteSession(fakeBin, final.SessionID, env); err != nil {
		t.Fatal("existing session:", err)
	}
	if len(sessionDirs(t, dir)) != 0 {
		t.Fatal("session still on disk")
	}
	if err := DeleteSession(fakeBin, final.SessionID, env); err != nil {
		t.Fatal("already deleted:", err)
	}
	if err := DeleteSession(fakeBin, "probe", env); err == nil {
		t.Fatal("invalid id accepted")
	}
	if got := deleted(t, dir); len(got) != 2 {
		t.Fatalf("grok ran for %v, want the two valid deletes only", got)
	}
	err = DeleteSession(fakeBin, NewSessionID(), append(env, "FAKEGROK_DELETE_FAIL=1"))
	if err == nil || !strings.Contains(err.Error(), "session store is locked") {
		t.Fatalf("failure not reported: %v", err)
	}
	if err := DeleteSession(filepath.Join(dir, "missing-grok"), NewSessionID(), env); err == nil {
		t.Fatal("missing executable not reported")
	}
}
