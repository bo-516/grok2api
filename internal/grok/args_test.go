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

// TestMediaCommandArgs keeps chat flags for an empty tool list and emits the
// media allowlist when Tools is set. Chat must not gain bypassPermissions.
func TestMediaCommandArgs(t *testing.T) {
	chat := strings.Join(CommandArgs("/p", "/c", RunSpec{}), " ")
	if !strings.Contains(chat, "--tools todo_write") || !strings.Contains(chat, "--disallowed-tools search_tool,use_tool,todo_write") || !strings.Contains(chat, "--permission-mode dontAsk") || !strings.Contains(chat, "--max-turns 1") {
		t.Fatal(chat)
	}
	media := strings.Join(CommandArgs("/p", "/mcwd", RunSpec{
		Tools: []string{"image_gen"}, MaxTurns: 2, PermissionMode: "bypassPermissions",
	}), " ")
	if !strings.Contains(media, "--tools image_gen") || !strings.Contains(media, "--disallowed-tools search_tool,use_tool") || !strings.Contains(media, "--permission-mode bypassPermissions") || !strings.Contains(media, "--max-turns 2") {
		t.Fatal(media)
	}
	if strings.Contains(media, "todo_write") {
		t.Fatal(media)
	}
	both := strings.Join(CommandArgs("/p", "/mcwd", RunSpec{Tools: []string{"image_gen", "reference_to_video"}, MaxTurns: 3, PermissionMode: "bypassPermissions"}), " ")
	if !strings.Contains(both, "--tools image_gen,reference_to_video") {
		t.Fatal(both)
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
