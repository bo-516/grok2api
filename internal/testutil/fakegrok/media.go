package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// mediaScenario reports whether scenario is played by runMedia instead of a fixture file.
func mediaScenario(name string) bool {
	switch name {
	case "image-gen", "image-edit", "video", "video-t2v", "video-reversed",
		"media-blocked", "media-tier", "media-evil-path", "media-extra-call",
		"media-bad-outside", "media-bad-symlink", "media-bad-text", "media-inline",
		"media-empty-tools", "media-extra-tools", "media-hang", "media-short":
		return true
	default:
		return false
	}
}

// mediaCall is one job-file call. Arguments is the raw object, including $OUTPUT_k.
type mediaCall struct {
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
}

// runMedia prints a media init line and, unless the scenario only checks init,
// the tool calls the job JSON asked for. sid is --session-id. Output files are
// written under GROK_HOME/sessions so the server's path check accepts them.
// A scenario that should be killed sleeps instead of exiting.
func runMedia(scenario string, args []string, body, sid string) error {
	if sid == "" {
		sid = "11111111-1111-4111-8111-111111111111"
	}
	tools := toolsJSON(flagValue(args, "--tools"))
	switch scenario {
	case "media-empty-tools":
		tools = "[]"
	case "media-extra-tools":
		tools = withExtra(tools, "run_terminal_command")
	}
	fmt.Printf("{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":%q,\"model\":\"grok-4.7\",\"permissionMode\":\"bypassPermissions\",\"tools\":%s}\n", sid, tools)
	_ = os.Stdout.Sync()
	switch scenario {
	case "media-empty-tools", "media-extra-tools":
		time.Sleep(30 * time.Second)
		return nil
	case "media-hang":
		time.Sleep(45 * time.Second)
		return nil
	case "video":
		time.Sleep(2 * time.Second)
	}
	calls := parseMediaCalls(body)
	if scenario == "video-reversed" && len(calls) > 1 {
		calls[0], calls[1] = calls[1], calls[0]
	}
	if scenario == "media-short" && len(calls) > 1 {
		calls = calls[:1]
	}
	var made []string
	for i, c := range calls {
		input := substituteOutputs(c.Arguments, made)
		if scenario == "media-evil-path" {
			input = json.RawMessage(`{"prompt":"x","image":["/etc/hosts"]}`)
		}
		id := fmt.Sprintf("call-%d", i)
		emitToolUse(sid, id, c.Tool, input)
		logMediaCall(c.Tool, input, "")
		if scenario == "media-evil-path" || scenario == "video-reversed" {
			time.Sleep(30 * time.Second)
			return nil
		}
		if scenario == "media-extra-call" && i == len(calls)-1 {
			emitToolUse(sid, "call-extra", c.Tool, input)
			time.Sleep(30 * time.Second)
			return nil
		}
		switch scenario {
		case "media-blocked":
			emitToolResult(sid, id, "content policy violation: blocked by moderation", true, nil)
			emitDone(sid, true)
			return nil
		case "media-tier":
			emitToolResult(sid, id, "Video generation requires an upgrade to a SuperGrok subscription", true, nil)
			emitDone(sid, true)
			return nil
		case "media-bad-outside":
			path, err := outsideFile()
			if err != nil {
				return err
			}
			emitToolResult(sid, id, pathJSON(c.Tool, path), false, nil)
			emitDone(sid, false)
			return nil
		case "media-bad-symlink":
			path, err := symlinkFile(args, sid)
			if err != nil {
				return err
			}
			emitToolResult(sid, id, pathJSON(c.Tool, path), false, nil)
			emitDone(sid, false)
			return nil
		case "media-bad-text":
			path, err := textFile(args, sid)
			if err != nil {
				return err
			}
			emitToolResult(sid, id, pathJSON(c.Tool, path), false, nil)
			emitDone(sid, false)
			return nil
		case "media-inline":
			emitToolResult(sid, id, "", false, sampleBytes("media-sample.png", samplePNG))
			emitDone(sid, false)
			return nil
		}
		path, err := writeOutput(args, sid, c.Tool, i+1)
		if err != nil {
			return err
		}
		made = append(made, path)
		logMediaCall(c.Tool, input, path)
		emitToolResult(sid, id, pathJSON(c.Tool, path), false, nil)
	}
	emitDone(sid, false)
	return nil
}

// parseMediaCalls reads the job JSON from the prompt file. A preface line is skipped.
// A body with no JSON object returns no calls.
func parseMediaCalls(body string) []mediaCall {
	i := strings.Index(body, "{")
	if i < 0 {
		return nil
	}
	var job struct {
		Calls []mediaCall `json:"calls"`
	}
	if json.Unmarshal([]byte(body[i:]), &job) != nil {
		return nil
	}
	return job.Calls
}

// substituteOutputs replaces $OUTPUT_k with the k-th produced path (k is 1-based).
// Paths are substituted as JSON strings so quotes in a path cannot break the object.
func substituteOutputs(raw json.RawMessage, made []string) json.RawMessage {
	s := string(raw)
	for i, p := range made {
		enc, _ := json.Marshal(p)
		s = strings.ReplaceAll(s, fmt.Sprintf("\"$OUTPUT_%d\"", i+1), string(enc))
	}
	return json.RawMessage(s)
}

// withExtra appends name to a tools JSON array.
func withExtra(tools, name string) string {
	var list []string
	_ = json.Unmarshal([]byte(tools), &list)
	list = append(list, name)
	b, err := json.Marshal(list)
	if err != nil {
		return tools
	}
	return string(b)
}

// emitToolUse writes the partial-stream tool_use and the assistant copy.
// The server dedupes them by id. input is a JSON object.
func emitToolUse(sid, id, name string, input json.RawMessage) {
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	fmt.Printf("{\"type\":\"stream_event\",\"event\":{\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":%q,\"name\":%q,\"input\":{}}},\"session_id\":%q}\n", id, name, sid)
	fmt.Printf("{\"type\":\"stream_event\",\"event\":{\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":%s}},\"session_id\":%q}\n", jsonString(string(input)), sid)
	fmt.Printf("{\"type\":\"stream_event\",\"event\":{\"type\":\"content_block_stop\",\"index\":1},\"session_id\":%q}\n", sid)
	fmt.Printf("{\"type\":\"assistant\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"tool_use\",\"id\":%q,\"name\":%q,\"input\":%s}]},\"session_id\":%q}\n", id, name, input, sid)
	_ = os.Stdout.Sync()
}

// emitToolResult writes one user tool_result. text is the result string.
// inline, when set, is sent as a base64 image block and text is ignored.
func emitToolResult(sid, id, text string, isErr bool, inline []byte) {
	var content any
	if inline != nil {
		content = []any{map[string]any{
			"type": "image",
			"source": map[string]any{
				"type":       "base64",
				"media_type": "image/png",
				"data":       base64.StdEncoding.EncodeToString(inline),
			},
		}}
	} else {
		content = text
	}
	msg := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type":        "tool_result",
				"tool_use_id": id,
				"is_error":    isErr,
				"content":     content,
			}},
		},
		"session_id": sid,
	}
	b, _ := json.Marshal(msg)
	fmt.Printf("%s\n", b)
	_ = os.Stdout.Sync()
}

// emitDone writes a terminal result line. isErr marks an error subtype.
func emitDone(sid string, isErr bool) {
	sub := "success"
	if isErr {
		sub = "error_during_execution"
	}
	fmt.Printf("{\"type\":\"result\",\"subtype\":%q,\"is_error\":%t,\"num_turns\":2,\"result\":\"DONE\",\"stop_reason\":\"end_turn\",\"session_id\":%q,\"usage\":{\"input_tokens\":12,\"output_tokens\":4}}\n", sub, isErr, sid)
	_ = os.Stdout.Sync()
}

// pathJSON is the ImageGen-shaped result string grok 1.0.41 returns.
func pathJSON(tool, path string) string {
	kind := "ImageGen"
	if tool == "reference_to_video" {
		kind = "VideoGen"
	}
	b, _ := json.Marshal(map[string]string{"type": kind, "path": path, "filename": filepath.Base(path)})
	return string(b)
}

// writeOutput stores the sample bytes in this run's session directory.
// The returned path contains sid, which the server requires.
func writeOutput(args []string, sid, tool string, n int) (string, error) {
	sub, name, payload := "images", fmt.Sprintf("%d.jpg", n), sampleBytes("media-sample.jpg", sampleJPEG)
	if tool == "reference_to_video" {
		sub, name, payload = "videos", fmt.Sprintf("%d.mp4", n), sampleBytes("media-sample.mp4", sampleMP4)
	}
	dir, err := sessionSub(args, sid, sub)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		return "", err
	}
	if rep := os.Getenv("FAKEGROK_REPORT"); rep != "" {
		_ = os.WriteFile(filepath.Join(filepath.Dir(rep), "player-"+name), payload, 0o600)
	}
	return path, nil
}

// sessionSub is GROK_HOME/sessions/<encoded cwd>/<sid>/<sub>, created at 0700.
func sessionSub(args []string, sid, sub string) (string, error) {
	root := storeRoot()
	if root == "" {
		home, _ := os.UserHomeDir()
		root = filepath.Join(home, ".grok", "sessions")
	}
	cwd := flagValue(args, "--cwd")
	real, err := filepath.EvalSymlinks(cwd)
	if err != nil || real == "" {
		real = cwd
	}
	dir := filepath.Join(root, groupName(real), sid, sub)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// outsideFile writes a png that is not under the session directory.
func outsideFile() (string, error) {
	f, err := os.CreateTemp("", "agent-mock-outside-*.png")
	if err != nil {
		return "", err
	}
	defer f.Close()
	_, err = f.Write(sampleBytes("media-sample.png", samplePNG))
	return f.Name(), err
}

// symlinkFile points at a real png from inside the session directory.
func symlinkFile(args []string, sid string) (string, error) {
	dir, err := sessionSub(args, sid, "images")
	if err != nil {
		return "", err
	}
	real := filepath.Join(dir, "real.png")
	if err := os.WriteFile(real, sampleBytes("media-sample.png", samplePNG), 0o600); err != nil {
		return "", err
	}
	link := filepath.Join(dir, "link.png")
	_ = os.Remove(link)
	if err := os.Symlink(real, link); err != nil {
		return "", err
	}
	return link, nil
}

// textFile writes a non-image into the session directory with a .png name.
func textFile(args []string, sid string) (string, error) {
	dir, err := sessionSub(args, sid, "images")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "note.png")
	return path, os.WriteFile(path, []byte("this is not an image"), 0o600)
}

// sampleBytes reads testdata or returns fallback. A short file uses fallback.
func sampleBytes(name string, fallback []byte) []byte {
	b, err := os.ReadFile(filepath.Join(fixtureDir(), name))
	if err != nil || len(b) < 8 {
		return fallback
	}
	return b
}

// logMediaCall appends one JSON line when FAKEGROK_CALLS is set.
// output is the file the player wrote, or empty when the call has no file yet.
func logMediaCall(name string, input json.RawMessage, output string) {
	p := os.Getenv("FAKEGROK_CALLS")
	if p == "" || output == "" && name == "" {
		return
	}
	if output == "" {
		return
	}
	rec := map[string]any{"name": name, "input": json.RawMessage(input), "output": output}
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

// jsonString JSON-encodes s for embedding as a JSON string value.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// samplePNG is a 1x1 PNG used when testdata is missing.
var samplePNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x02, 0x00, 0x00, 0x00, 0x90, 0x77, 0x53,
	0xde, 0x00, 0x00, 0x00, 0x0c, 0x49, 0x44, 0x41, 0x54, 0x08, 0xd7, 0x63, 0xf8, 0xcf, 0xc0, 0x00,
	0x00, 0x00, 0x03, 0x00, 0x01, 0x00, 0x05, 0xfe, 0x02, 0xfe, 0xdc, 0xcc, 0x59, 0xe7, 0x00, 0x00,
	0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82,
}

// sampleJPEG is a tiny JPEG used when testdata is missing.
var sampleJPEG = []byte{
	0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 0x4a, 0x46, 0x49, 0x46, 0x00, 0x01, 0x01, 0x00, 0x00, 0x01,
	0x00, 0x01, 0x00, 0x00, 0xff, 0xdb, 0x00, 0x43, 0x00, 0x08, 0x06, 0x06, 0x07, 0x06, 0x05, 0x08,
	0x07, 0x07, 0x07, 0x09, 0x09, 0x08, 0x0a, 0x0c, 0x14, 0x0d, 0x0c, 0x0b, 0x0b, 0x0c, 0x19, 0x12,
	0x13, 0x0f, 0x14, 0x1d, 0x1a, 0x1f, 0x1e, 0x1d, 0x1a, 0x1c, 0x1c, 0x20, 0x24, 0x2e, 0x27, 0x20,
	0x22, 0x2c, 0x23, 0x1c, 0x1c, 0x28, 0x37, 0x29, 0x2c, 0x30, 0x31, 0x34, 0x34, 0x34, 0x1f, 0x27,
	0x39, 0x3d, 0x38, 0x32, 0x3c, 0x2e, 0x33, 0x34, 0x32, 0xff, 0xc0, 0x00, 0x0b, 0x08, 0x00, 0x01,
	0x00, 0x01, 0x01, 0x01, 0x11, 0x00, 0xff, 0xc4, 0x00, 0x14, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x09, 0xff, 0xc4, 0x00, 0x14,
	0x10, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0xff, 0xda, 0x00, 0x08, 0x01, 0x01, 0x00, 0x00, 0x3f, 0x00, 0x7f, 0x00, 0xff, 0xd9,
}

// sampleMP4 is an ftyp box plus padding so a Range of 100 bytes succeeds.
var sampleMP4 = func() []byte {
	b := make([]byte, 200)
	copy(b, []byte{0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm', 0, 0, 0, 0, 'i', 's', 'o', 'm', 'm', 'p', '4', '1'})
	return b
}()
