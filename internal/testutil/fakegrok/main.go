// Command fakegrok replays recorded grok stdout for tests.
// It is a real subprocess. Tests point -grok-bin at it. It is not a handler mock.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// probePrompt matches grok.ProbePrompt. A prompt file with this body prints init and waits to be killed.
const probePrompt = "agent-mock-toolset-probe"

// main dispatches version, models, sessions, the toolset probe, and fixture replay.
// SIGTERM is ignored so agent-mock's 5s SIGKILL path is what actually ends a hung run.
func main() {
	signalIgnore()
	args := os.Args[1:]
	if has(args, "--version") || has(args, "-v") || (len(args) > 0 && args[0] == "version") {
		fmt.Println("grok 1.0.41 (fixture)")
		return
	}
	if len(args) > 0 && args[0] == "models" {
		if os.Getenv("FAKEGROK_LOGIN") == "no" {
			fmt.Print(unauthModels)
			return
		}
		fmt.Print(okModels)
		return
	}
	if len(args) > 0 && args[0] == "sessions" {
		sessions(args[1:])
		return
	}
	promptPath := flagValue(args, "--prompt-file")
	body := readFile(promptPath)
	writePID()
	writeReport(args, promptPath, body)
	if strings.TrimSpace(body) == probePrompt {
		tools := os.Getenv("FAKEGROK_PROBE_TOOLS")
		if tools == "" {
			tools = "[]"
		}
		fmt.Printf("{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"probe\",\"model\":\"grok-4.7-build-fast\",\"permissionMode\":\"dontAsk\",\"tools\":%s}\n", tools)
		_ = os.Stdout.Sync()
		time.Sleep(30 * time.Second)
		return
	}
	if d := os.Getenv("FAKEGROK_START_DELAY"); d != "" {
		if wait, err := time.ParseDuration(d); err == nil {
			time.Sleep(wait)
		}
	}
	scenario := nextScenario()
	model := flagValue(args, "-m")
	hold, _ := time.ParseDuration(os.Getenv("FAKEGROK_HOLD"))
	switch scenario {
	case "silent":
		time.Sleep(45 * time.Second)
	case "hang":
		fmt.Printf("{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"hang\",\"model\":%q,\"permissionMode\":\"dontAsk\",\"tools\":[]}\n", modelOr(model))
		fmt.Printf("{\"type\":\"stream_event\",\"event\":{\"type\":\"message_start\",\"message\":{\"model\":%q}},\"session_id\":\"hang\"}\n", modelOr(model))
		_ = os.Stdout.Sync()
		time.Sleep(45 * time.Second)
	case "unsafe":
		fmt.Printf("{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"unsafe\",\"model\":%q,\"permissionMode\":\"dontAsk\",\"tools\":[\"run_terminal_command\"]}\n", modelOr(model))
		_ = os.Stdout.Sync()
		time.Sleep(30 * time.Second)
	case "no-init":
		// A result record with no preceding system/init. Stays alive so the server must kill the group.
		fmt.Println(`{"type":"result","result":"pong","stop_reason":"end_turn","session_id":"abc-def"}`)
		_ = os.Stdout.Sync()
		time.Sleep(30 * time.Second)
	case "truncated":
		// Valid init and a text delta, then a cut-off JSON value, then exit.
		fmt.Printf("{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"trunc\",\"model\":%q,\"permissionMode\":\"dontAsk\",\"tools\":[]}\n", modelOr(model))
		fmt.Printf("{\"type\":\"stream_event\",\"event\":{\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}},\"session_id\":\"trunc\"}\n")
		fmt.Print("{")
		_ = os.Stdout.Sync()
	default:
		path := filepath.Join(fixtureDir(), fixtureName(scenario))
		replay(path, model, hold)
	}
}

// signalIgnore ignores SIGTERM. SIGKILL still ends the process.
func signalIgnore() {
	signal.Ignore(syscall.SIGTERM)
}

// sessions implements `grok sessions delete <id>` without reading stdin.
// Delete appends the id to FAKEGROK_DELETE_LOG when that path is set.
func sessions(args []string) {
	if len(args) >= 2 && args[0] == "delete" {
		id := args[1]
		fmt.Printf("Deleted session %s\n", id)
		if p := os.Getenv("FAKEGROK_DELETE_LOG"); p != "" {
			f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			if err == nil {
				fmt.Fprintln(f, id)
				_ = f.Close()
			}
		}
		return
	}
	fmt.Println("SESSION ID")
}

// replay writes a fixture to stdout. -m replaces the recorded default model id
// so the server reports the model grok was asked to run. hold sleeps after message_start.
func replay(path, model string, hold time.Duration) {
	b, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakegrok fixture: %v\n", err)
		os.Exit(1)
	}
	if model != "" {
		b = bytes.ReplaceAll(b, []byte("grok-4.7-build-fast"), []byte(model))
	}
	if hold <= 0 {
		_, _ = os.Stdout.Write(b)
		if !bytes.HasSuffix(b, []byte("\n")) {
			_, _ = os.Stdout.Write([]byte("\n"))
		}
		_ = os.Stdout.Sync()
		return
	}
	for _, line := range bytes.Split(b, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		_, _ = os.Stdout.Write(append(line, '\n'))
		_ = os.Stdout.Sync()
		if bytes.Contains(line, []byte("message_start")) {
			time.Sleep(hold)
		}
	}
}

// nextScenario returns the next comma-separated FAKEGROK_SCENARIO entry.
// FAKEGROK_SEQ is an exclusive counter so each prompt run advances, while the
// startup probe does not because it returns before this function.
func nextScenario() string {
	spec := os.Getenv("FAKEGROK_SCENARIO")
	if spec == "" {
		spec = "text"
	}
	parts := strings.Split(spec, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	idx := 0
	if seq := os.Getenv("FAKEGROK_SEQ"); seq != "" {
		f, err := os.OpenFile(seq, os.O_CREATE|os.O_RDWR, 0o600)
		if err == nil {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
			buf, _ := io.ReadAll(f)
			fmt.Sscanf(string(buf), "%d", &idx)
			_ = f.Truncate(0)
			_, _ = f.Seek(0, 0)
			fmt.Fprintf(f, "%d", idx+1)
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			_ = f.Close()
		}
	}
	if idx < 0 {
		idx = 0
	}
	if idx >= len(parts) {
		idx = len(parts) - 1
	}
	return parts[idx]
}

// fixtureName maps a scenario to a file under the fixture directory.
func fixtureName(scenario string) string {
	switch scenario {
	case "text":
		return "text.json"
	case "stream":
		return "stream.ndjson"
	case "schema":
		return "schema.ndjson"
	case "tools":
		return "tools.ndjson"
	case "bad-model":
		return "bad-model.ndjson"
	case "not-logged-in":
		return "not-logged-in.ndjson"
	case "rate-limit":
		return "rate-limit.json"
	case "usage-limit":
		return "usage-limit.json"
	case "verbatim":
		return "verbatim.ndjson"
	case "followup":
		return "followup.ndjson"
	case "stream-error":
		return "stream-error.ndjson"
	default:
		return scenario
	}
}

// fixtureDir is FAKEGROK_FIXTURE_DIR or testdata/grok next to the working directory.
func fixtureDir() string {
	if d := os.Getenv("FAKEGROK_FIXTURE_DIR"); d != "" {
		return d
	}
	return "testdata/grok"
}

// writePID writes the process id when FAKEGROK_PIDFILE is set.
func writePID() {
	p := os.Getenv("FAKEGROK_PIDFILE")
	if p == "" {
		return
	}
	_ = os.WriteFile(p, []byte(strconv.Itoa(os.Getpid())), 0o600)
}

// writeReport appends one JSON line describing this invocation.
// Tests assert the restricted argv, the absence of XAI_API_KEY, and the prompt file mode.
func writeReport(args []string, promptPath, body string) {
	p := os.Getenv("FAKEGROK_REPORT")
	if p == "" {
		return
	}
	mode := 0
	if st, err := os.Stat(promptPath); err == nil {
		mode = int(st.Mode().Perm())
	}
	_, xai := os.LookupEnv("XAI_API_KEY")
	rec := map[string]any{
		"argv":   args,
		"xai":    xai,
		"prompt": promptPath,
		"mode":   mode,
		"head":   trimHead(body),
		"body":   body,
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = f.Write(append(b, '\n'))
	_ = f.Close()
}

// trimHead returns the first 80 runes of the prompt so reports stay small.
func trimHead(s string) string {
	if len(s) > 80 {
		return s[:80]
	}
	return s
}

// flagValue returns the argument after name, or "".
func flagValue(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// has reports whether args contains want.
func has(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// readFile returns the file body or empty when path is empty or unreadable.
func readFile(path string) string {
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// modelOr uses the requested model or the recorded default.
func modelOr(model string) string {
	if model == "" {
		return "grok-4.7-build-fast"
	}
	return model
}

// okModels is the logged-in `grok models` text captured from grok 1.0.41.
const okModels = `You are logged in with grok.com.

Default model: grok-4.7-build-fast

Available models:
  - grok-4.7
  * grok-4.7-build-fast (default)
  - grok-4.6
  - grok-4.5
`

// unauthModels is the empty-GROK_HOME `grok models` text from grok 1.0.41.
const unauthModels = `You are not authenticated.

Default model: grok-4.6

Available models:
  * grok-4.6 (default)
  - grok-4.5
`
