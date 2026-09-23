package grok

import (
	"strings"
	"testing"
)

// TestCommandArgs locks the restricted flag set that grok 1.0.41 needs for tools:[].
func TestCommandArgs(t *testing.T) {
	args := CommandArgs("/tmp/p.txt", "/tmp/cwd", RunSpec{
		SystemOverride:  "sys",
		Model:           "grok-4.7",
		ReasoningEffort: "low",
		JSONSchema:      []byte(`{"type":"object"}`),
	})
	got := strings.Join(args, "\n")
	for _, want := range []string{
		"--prompt-file\n/tmp/p.txt",
		"--verbatim",
		"--output-format\nstreaming-messages-json",
		"--include-partial-messages",
		"--max-turns\n1",
		"--no-subagents",
		"--no-plan",
		"--disable-web-search",
		"--tools\ntodo_write",
		"--disallowed-tools\nsearch_tool,use_tool,todo_write",
		"--permission-mode\ndontAsk",
		"--cwd\n/tmp/cwd",
		"--system-prompt-override\nsys",
		"-m\ngrok-4.7",
		"--reasoning-effort\nlow",
		"--json-schema\n{\"type\":\"object\"}",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in\n%s", want, got)
		}
	}
	if strings.Contains(got, "bypassPermissions") || strings.Contains(got, "run_terminal_command") {
		t.Fatal(got)
	}
	bare := strings.Join(CommandArgs("/p", "/c", RunSpec{}), " ")
	if strings.Contains(bare, "--json-schema") || strings.Contains(bare, " -m ") || strings.Contains(bare, "--reasoning-effort") || strings.Contains(bare, "--system-prompt-override") {
		t.Fatal(bare)
	}
}

// TestChildEnv strips XAI_API_KEY and forces the restricted grok variables.
func TestChildEnv(t *testing.T) {
	env := ChildEnv([]string{"PATH=/bin", "XAI_API_KEY=secret", "GROK_MEMORY=1", "HOME=/tmp"})
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "XAI_API_KEY") {
		t.Fatal(joined)
	}
	if !strings.Contains(joined, "GROK_MEMORY=0") || !strings.Contains(joined, "GROK_SUBAGENTS=0") || !strings.Contains(joined, "GROK_DISABLE_AUTOUPDATER=1") {
		t.Fatal(joined)
	}
	if !strings.Contains(joined, "PATH=/bin") || !strings.Contains(joined, "HOME=/tmp") {
		t.Fatal(joined)
	}
}
