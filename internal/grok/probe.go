package grok

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Model is one model id discovered from `grok models`.
type Model struct {
	// ID is the grok model id, for example grok-4.7-build-fast.
	ID string
	// Default is true for the model grok marks as its default.
	Default bool
}

// ProbeResult is the startup self-check. LoginOK false still lets the process listen.
// ToolsetEmpty is true only after a real init record with tools:[].
type ProbeResult struct {
	// Bin is the executable that was probed.
	Bin string
	// Version is the numeric grok version, such as 1.0.41. Empty if --version failed.
	Version string
	// VersionWarn is set when Version is present and older than 1.0.41.
	VersionWarn string
	// LoginOK is true when `grok models` says the user is logged in.
	LoginOK bool
	// Account is the host from "logged in with grok.com", or empty.
	Account string
	// Models is the model list. It can be non-empty even when LoginOK is false;
	// grok 1.0.41 still prints a short list when it is not authenticated.
	Models []Model
	// DefaultModel is the model id grok would use when -m is omitted.
	DefaultModel string
	// ToolsetEmpty is true when the probe init record had tools:[].
	ToolsetEmpty bool
	// ToolsetError is why the toolset could not be confirmed. Empty on success.
	ToolsetError string
}

// Probe runs `grok --version`, `grok models`, and a toolset probe that is killed at init.
// ctx bounds the whole check. A failure does not stop the caller from listening:
// the result describes what was found. bin is the grok executable.
func Probe(ctx context.Context, bin string, runner *Runner) ProbeResult {
	if bin == "" {
		bin = "grok"
	}
	out := ProbeResult{Bin: bin}
	verOut, verErr := runCapture(ctx, bin, runner, "--version")
	out.Version = ParseVersion(verOut)
	if out.Version == "" && verErr != nil {
		out.VersionWarn = "grok --version failed: " + verErr.Error()
	} else if out.Version != "" && OlderThan(out.Version, "1.0.41") {
		out.VersionWarn = "grok " + out.Version + " is older than 1.0.41; agent-mock was verified on 1.0.41"
	}
	modelsOut, _ := runCapture(ctx, bin, runner, "models")
	out.LoginOK, out.Account, out.Models, out.DefaultModel = ParseModels(modelsOut)
	pctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	_, err := runner.Run(pctx, RunSpec{Prompt: ProbePrompt, StopAfterInit: true}, Events{})
	if err != nil {
		out.ToolsetError = err.Error()
		return out
	}
	out.ToolsetEmpty = true
	return out
}

// runCapture executes bin with args and returns combined output.
// The child environment has XAI_API_KEY removed. A deadline error returns the partial output.
func runCapture(ctx context.Context, bin string, runner *Runner, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, bin, args...)
	parent := os.Environ()
	if runner != nil && runner.Environ != nil {
		parent = runner.Environ()
	}
	cmd.Env = ChildEnv(parent)
	b, err := cmd.CombinedOutput()
	return string(b), err
}

// ParseVersion extracts the leading major.minor.patch from `grok 1.0.41 (hash)`.
// An unparseable string returns empty, and the caller warns instead of refusing to start.
func ParseVersion(out string) string {
	fields := strings.Fields(out)
	for i, f := range fields {
		if f == "grok" && i+1 < len(fields) {
			v := strings.Trim(fields[i+1], "v")
			if _, ok := splitVersion(v); ok {
				return v
			}
		}
	}
	return ""
}

// OlderThan reports whether version a is strictly older than b.
// Unparseable versions return false so a weird string does not force a warning.
func OlderThan(a, b string) bool {
	ap, aok := splitVersion(a)
	bp, bok := splitVersion(b)
	if !aok || !bok {
		return false
	}
	for i := 0; i < 3; i++ {
		if ap[i] != bp[i] {
			return ap[i] < bp[i]
		}
	}
	return false
}

// splitVersion parses major.minor.patch. Extra suffixes after a hyphen are ignored.
func splitVersion(v string) ([3]int, bool) {
	v, _, _ = strings.Cut(v, "-")
	parts := strings.Split(v, ".")
	if len(parts) < 3 {
		return [3]int{}, false
	}
	var n [3]int
	for i := 0; i < 3; i++ {
		x, err := strconv.Atoi(parts[i])
		if err != nil {
			return [3]int{}, false
		}
		n[i] = x
	}
	return n, true
}

// ParseModels reads `grok models` text.
// "not authenticated", "not signed in", and "not logged" mean logged out even
// when a model list is still printed. "logged in with X" sets the account to X.
// Bullet lines ("- id" and "* id (default)") are models. The return values are
// loginOK, account, models, defaultModel.
func ParseModels(out string) (bool, string, []Model, string) {
	lower := strings.ToLower(out)
	loginOK := false
	switch {
	case strings.Contains(lower, "not authenticated"), strings.Contains(lower, "not signed in"), strings.Contains(lower, "not logged"):
		loginOK = false
	case strings.Contains(lower, "logged in"):
		loginOK = true
	}
	account := ""
	if i := strings.Index(lower, "logged in with "); i >= 0 {
		rest := out[i+len("logged in with "):]
		rest = strings.TrimSpace(strings.Split(rest, "\n")[0])
		rest = strings.TrimRight(rest, ".")
		account = rest
	}
	var models []Model
	def := ""
	for _, line := range strings.Split(out, "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(trim), "default model:") {
			def = strings.TrimSpace(trim[len("default model:"):])
			continue
		}
		if strings.HasPrefix(trim, "* ") || strings.HasPrefix(trim, "- ") {
			id := strings.TrimSpace(trim[2:])
			isDef := strings.Contains(id, "(default)")
			id = strings.TrimSpace(strings.TrimSuffix(id, "(default)"))
			if id == "" {
				continue
			}
			models = append(models, Model{ID: id, Default: isDef})
			if isDef {
				def = id
			}
		}
	}
	if def != "" {
		found := false
		for i := range models {
			if models[i].ID == def {
				models[i].Default = true
				found = true
			}
		}
		if !found {
			models = append(models, Model{ID: def, Default: true})
		}
	}
	return loginOK, account, models, def
}

// FormatStartup renders the banner operators paste into a backend dev config.
// baseURL is the OpenAI base, including /v1. apiKeyHint is the bearer the backend
// should send; loopback without a configured key uses "dev".
func FormatStartup(version, baseURL, apiKeyHint string, maxConc int, p ProbeResult) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "agent-mock v%s  (local dev only)\n", version)
	ver := p.Version
	if ver == "" {
		ver = "unknown"
	}
	fmt.Fprintf(&b, "grok      %s (%s)\n", p.Bin, ver)
	if p.VersionWarn != "" {
		fmt.Fprintf(&b, "warning   %s\n", p.VersionWarn)
	}
	if p.LoginOK {
		acct := p.Account
		if acct == "" {
			acct = "grok.com"
		}
		fmt.Fprintf(&b, "login     ok (%s)\n", acct)
	} else {
		fmt.Fprintf(&b, "login     NOT logged in - run grok login\n")
	}
	fmt.Fprintf(&b, "models    %s\n", formatModelList(p.Models, p.DefaultModel))
	if p.ToolsetEmpty {
		fmt.Fprintf(&b, "toolset   [] (checked at startup and on every run), permission-mode=dontAsk\n")
	} else {
		detail := p.ToolsetError
		if detail == "" {
			detail = "not checked"
		}
		fmt.Fprintf(&b, "toolset   unchecked (%s), permission-mode=dontAsk\n", detail)
	}
	fmt.Fprintf(&b, "listen    %s  (max %d concurrent grok runs)\n", baseURL, maxConc)
	if apiKeyHint == "" {
		apiKeyHint = "dev"
	}
	fmt.Fprintf(&b, "backend   OPENAI_BASE_URL=%s  OPENAI_API_KEY=%s\n", baseURL, apiKeyHint)
	fmt.Fprintf(&b, "doc       %s/doc\n", strings.TrimSuffix(baseURL, "/v1"))
	return b.String()
}

// formatModelList renders "grok-4.7, grok-4.7-build-fast (default)".
// An empty list renders "(none)" so the banner still has a models line.
func formatModelList(models []Model, def string) string {
	if len(models) == 0 {
		if def != "" {
			return def + " (default)"
		}
		return "(none)"
	}
	parts := make([]string, 0, len(models))
	for _, m := range models {
		if m.Default || m.ID == def {
			parts = append(parts, m.ID+" (default)")
			continue
		}
		parts = append(parts, m.ID)
	}
	return strings.Join(parts, ", ")
}
