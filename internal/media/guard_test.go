package media

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaoboli/agent-mock/internal/grok"
)

// TestGuardRejects kills an outside path, an extra call, a non-prompt mismatch,
// and a bad output path, and accepts an inline image.
func TestGuardRejects(t *testing.T) {
	dir := t.TempDir()
	st, err := NewStore(filepath.Join(dir, "store"), 0)
	if err != nil {
		t.Fatal(err)
	}
	png := pngBytes(t)
	staged := filepath.Join(dir, "in.png")
	if err := os.WriteFile(staged, png, 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanImage(ImageJob{Prompt: "draw", N: 1, Aspect: "16:9", Edit: true, Inputs: []string{staged}, MCWD: dir})
	if err != nil {
		t.Fatal(err)
	}
	sessions := filepath.Join(dir, "sessions")
	g := NewGuard(plan, st, sessions)
	g.SetSession("sid")
	evil, _ := json.Marshal(map[string]any{"prompt": "draw", "image": []string{"/etc/hosts"}})
	err = g.OnToolUse(grok.ToolUse{ID: "1", Name: "image_edit", Input: evil})
	ge, ok := grok.AsError(err)
	if !ok || ge.Code != grok.CodeUnsafeInput || st.Count() != 0 {
		t.Fatalf("evil %v count %d", err, st.Count())
	}

	plan, _ = PlanImage(ImageJob{Prompt: "draw", N: 1, Aspect: "16:9", MCWD: dir})
	g = NewGuard(plan, st, sessions)
	okInput, _ := json.Marshal(map[string]any{"prompt": "draw", "aspect_ratio": "16:9"})
	if err := g.OnToolUse(grok.ToolUse{ID: "a", Name: "image_gen", Input: okInput}); err != nil {
		t.Fatal(err)
	}
	err = g.OnToolUse(grok.ToolUse{ID: "b", Name: "image_gen", Input: okInput})
	ge, ok = grok.AsError(err)
	if !ok || ge.Code != grok.CodeBadOutput {
		t.Fatal(err)
	}

	g = NewGuard(plan, st, sessions)
	badAspect, _ := json.Marshal(map[string]any{"prompt": "changed", "aspect_ratio": "1:1"})
	err = g.OnToolUse(grok.ToolUse{ID: "c", Name: "image_gen", Input: badAspect})
	ge, ok = grok.AsError(err)
	if !ok || ge.Code != grok.CodeBadOutput {
		t.Fatal(err)
	}

	g = NewGuard(plan, st, sessions)
	if err := g.OnToolUse(grok.ToolUse{ID: "d", Name: "image_gen", Input: okInput}); err != nil {
		t.Fatal(err)
	}
	err = g.OnToolResult(grok.ToolResult{ToolUseID: "d", Text: `{"type":"ImageGen","path":"/etc/passwd"}`})
	ge, ok = grok.AsError(err)
	if !ok || ge.Code != grok.CodeBadOutput || st.Count() != 0 {
		t.Fatalf("path %v count %d", err, st.Count())
	}

	g = NewGuard(plan, st, sessions)
	if err := g.OnToolUse(grok.ToolUse{ID: "e", Name: "image_gen", Input: okInput}); err != nil {
		t.Fatal(err)
	}
	err = g.OnToolResult(grok.ToolResult{ToolUseID: "e", Inline: []grok.InlineMedia{{MediaType: "image/png", Data: png}}})
	if err != grok.ErrStop || st.Count() != 1 {
		t.Fatalf("inline %v count %d", err, st.Count())
	}
}

// TestVideoOrderFails when reference_to_video runs before image_gen's output exists.
func TestVideoOrderFails(t *testing.T) {
	plan, err := PlanVideo(VideoJob{
		Prompt: "waves", Duration: 8, Aspect: "16:9", Resolution: "480p", TextToVideo: true, MCWD: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Spec.MaxTurns != 3 || plan.Spec.Tools[0] != "image_gen" {
		t.Fatalf("%+v", plan.Spec)
	}
	st, _ := NewStore(filepath.Join(t.TempDir(), "s"), 0)
	g := NewGuard(plan, st, t.TempDir())
	raw := plan.Calls[1].Args
	b, _ := json.Marshal(raw)
	err = g.OnToolUse(grok.ToolUse{ID: "v", Name: "reference_to_video", Input: b})
	ge, ok := grok.AsError(err)
	if !ok || ge.Code != grok.CodeBadOutput {
		t.Fatal(err)
	}
}

// TestGuardRejectsIllegalPathBeforeCount requires an outside path to be
// unsafe_grok_tool_input even when the list is longer than the plan.
// A length check that runs first returns grok_bad_output and fails this test.
// The same staged path twice is a count mismatch and stays grok_bad_output.
// Nothing is stored. The runner SIGKILLs when OnToolUse returns unsafe input.
func TestGuardRejectsIllegalPathBeforeCount(t *testing.T) {
	dir := t.TempDir()
	st, err := NewStore(filepath.Join(dir, "store"), 0)
	if err != nil {
		t.Fatal(err)
	}
	png := pngBytes(t)
	staged := filepath.Join(dir, "in.png")
	if err := os.WriteFile(staged, png, 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanImage(ImageJob{Prompt: "draw", N: 1, Edit: true, Inputs: []string{staged}, MCWD: dir})
	if err != nil {
		t.Fatal(err)
	}
	sessions := filepath.Join(dir, "sessions")
	g := NewGuard(plan, st, sessions)
	g.SetSession("sid")
	evil, err := json.Marshal(map[string]any{"prompt": "draw", "image": []string{staged, "/etc/hosts"}})
	if err != nil {
		t.Fatal(err)
	}
	err = g.OnToolUse(grok.ToolUse{ID: "1", Name: "image_edit", Input: evil})
	ge, ok := grok.AsError(err)
	if !ok || ge.Code != grok.CodeUnsafeInput || st.Count() != 0 {
		t.Fatalf("evil %v count %d", err, st.Count())
	}

	g = NewGuard(plan, st, sessions)
	g.SetSession("sid")
	dup, err := json.Marshal(map[string]any{"prompt": "draw", "image": []string{staged, staged}})
	if err != nil {
		t.Fatal(err)
	}
	err = g.OnToolUse(grok.ToolUse{ID: "2", Name: "image_edit", Input: dup})
	ge, ok = grok.AsError(err)
	if !ok || ge.Code != grok.CodeBadOutput || st.Count() != 0 {
		t.Fatalf("dup %v count %d", err, st.Count())
	}

	plan, err = PlanVideo(VideoJob{
		Prompt: "pan", Duration: 8, Aspect: "16:9", Resolution: "480p",
		First: staged, Frames: []Keyframe{{Path: staged, Timestamp: 1}}, MCWD: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	g = NewGuard(plan, st, sessions)
	g.SetSession("sid")
	var in map[string]any
	raw, err := json.Marshal(plan.Calls[0].Args)
	if err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(raw, &in) != nil {
		t.Fatal(string(raw))
	}
	frames, _ := in["keyframes"].([]any)
	in["keyframes"] = append(frames, map[string]any{"image": "/etc/hosts", "timestamp_s": 1.0})
	body, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	err = g.OnToolUse(grok.ToolUse{ID: "k", Name: "reference_to_video", Input: body})
	ge, ok = grok.AsError(err)
	if !ok || ge.Code != grok.CodeUnsafeInput || st.Count() != 0 {
		t.Fatalf("frame %v count %d", err, st.Count())
	}
}

// TestGuardRejectsOversizedImageNamedMP4 stores nothing when a tool result
// path ends in .mp4 but sniffs as a PNG larger than 50 MiB. The cap follows
// the sniffed type. A suffix check would accept it under the 500 MiB video cap.
func TestGuardRejectsOversizedImageNamedMP4(t *testing.T) {
	dir := t.TempDir()
	st, err := NewStore(filepath.Join(dir, "store"), 0)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := PlanImage(ImageJob{Prompt: "draw", N: 1, Aspect: "1:1", MCWD: dir})
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "sessions")
	sid := "sid"
	path := filepath.Join(root, "group", sid, "big.mp4")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	// pngSig is the 8-byte signature sniff accepts. The rest of the file is a hole.
	pngSig := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	if _, err := f.Write(pngSig); err != nil {
		t.Fatal(err)
	}
	// One byte past the 50 MiB image cap. The hole is not filled.
	if _, err := f.Seek(50<<20, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte{0}); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	g := NewGuard(plan, st, root)
	g.SetSession(sid)
	okInput, err := json.Marshal(map[string]any{"prompt": "draw", "aspect_ratio": "1:1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := g.OnToolUse(grok.ToolUse{ID: "d", Name: "image_gen", Input: okInput}); err != nil {
		t.Fatal(err)
	}
	res, err := json.Marshal(map[string]string{"type": "ImageGen", "path": path})
	if err != nil {
		t.Fatal(err)
	}
	err = g.OnToolResult(grok.ToolResult{ToolUseID: "d", Text: string(res)})
	ge, ok := grok.AsError(err)
	if !ok || ge.Code != grok.CodeBadOutput || !strings.Contains(ge.Message, "size limit") || st.Count() != 0 {
		t.Fatalf("err %v count %d", err, st.Count())
	}
}
