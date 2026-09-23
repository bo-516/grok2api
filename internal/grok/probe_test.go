package grok

import (
	"os"
	"strings"
	"testing"
)

// TestParseModels reads the logged-in and unauthenticated captures.
func TestParseModels(t *testing.T) {
	ok, err := os.ReadFile("../../testdata/grok/models.txt")
	if err != nil {
		t.Fatal(err)
	}
	login, account, models, def := ParseModels(string(ok))
	if !login || account != "grok.com" || def != "grok-4.7-build-fast" || len(models) != 4 {
		t.Fatalf("login %v account %s def %s models %+v", login, account, def, models)
	}
	raw, err := os.ReadFile("../../testdata/grok/models-unauthenticated.txt")
	if err != nil {
		t.Fatal(err)
	}
	login, _, models, def = ParseModels(string(raw))
	if login || def != "grok-4.6" || len(models) != 2 {
		t.Fatalf("unauth login %v def %s %+v", login, def, models)
	}
}

// TestVersion compares the verified version against older builds.
func TestVersion(t *testing.T) {
	if ParseVersion("grok 1.0.41 (4220f3b224a6)") != "1.0.41" {
		t.Fatal(ParseVersion("grok 1.0.41 (4220f3b224a6)"))
	}
	if !OlderThan("1.0.40", "1.0.41") || OlderThan("1.0.41", "1.0.41") || OlderThan("1.1.0", "1.0.41") {
		t.Fatal("version compare")
	}
}

// TestFormatStartup includes the lines the launch check looks for.
func TestFormatStartup(t *testing.T) {
	text := FormatStartup("0.1.0", "http://127.0.0.1:8787/v1", "", 4, ProbeResult{
		Bin: "/usr/bin/grok", Version: "1.0.41", LoginOK: true, Account: "grok.com",
		Models:       []Model{{ID: "grok-4.7"}, {ID: "grok-4.7-build-fast", Default: true}},
		DefaultModel: "grok-4.7-build-fast", ToolsetEmpty: true,
	})
	for _, s := range []string{"agent-mock v0.1.0", "/usr/bin/grok", "1.0.41", "login     ok (grok.com)", "grok-4.7-build-fast (default)", "toolset   []", "permission-mode=dontAsk", "http://127.0.0.1:8787/v1", "OPENAI_BASE_URL=", "OPENAI_API_KEY=dev", "doc       http://127.0.0.1:8787/doc"} {
		if !strings.Contains(text, s) {
			t.Fatalf("missing %q in\n%s", s, text)
		}
	}
	out := FormatStartup("0.1.0", "http://127.0.0.1:9/v1", "s3cret", 1, ProbeResult{VersionWarn: "grok 1.0.40 is older than 1.0.41; agent-mock was verified on 1.0.41"})
	if !strings.Contains(out, "NOT logged in - run grok login") || !strings.Contains(out, "warning") || !strings.Contains(out, "OPENAI_API_KEY=s3cret") {
		t.Fatal(out)
	}
}
