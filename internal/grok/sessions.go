package grok

import (
	"context"
	"os/exec"
	"time"
)

// DeleteSession runs `grok sessions delete id` with stdin closed.
// grok 1.0.41 prints "Deleted session <id>" and exits 0 without a prompt.
// The call is best-effort: a failure leaves the session on disk and does not
// change the HTTP response. parent is the environment before XAI_API_KEY is stripped.
func DeleteSession(bin, id string, parent []string) {
	if bin == "" || id == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "sessions", "delete", id)
	cmd.Env = ChildEnv(parent)
	cmd.Stdin = nil
	_, _ = cmd.CombinedOutput()
}
