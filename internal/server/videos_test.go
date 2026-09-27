package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestVideoDone posts a first-frame job and downloads the mp4 with a Range.
func TestVideoDone(t *testing.T) {
	h := start(t, startOpt{scenario: "video", media: true, videoTimeout: time.Minute})
	body := `{"prompt":"pan","image":{"url":"data:image/png;base64,` + b64PNG(t) + `"},"duration":6,"aspect_ratio":"16:9","resolution":"480p"}`
	res := postMedia(t, h, "/v1/videos/generations", body)
	if res.code != 200 {
		t.Fatal(string(res.body))
	}
	var ack struct {
		ID string `json:"request_id"`
	}
	if json.Unmarshal(res.body, &ack) != nil || ack.ID == "" {
		t.Fatal(string(res.body))
	}
	deadline := time.Now().Add(8 * time.Second)
	var done map[string]any
	for time.Now().Before(deadline) {
		gr, err := http.Get(h.ts.URL + "/v1/videos/" + ack.ID)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(gr.Body)
		gr.Body.Close()
		_ = json.Unmarshal(b, &done)
		if done["status"] == "done" || done["status"] == "failed" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if done["status"] != "done" {
		t.Fatal(done)
	}
	video := done["video"].(map[string]any)
	if int(video["duration"].(float64)) != 6 {
		t.Fatal(video)
	}
	req, _ := http.NewRequest(http.MethodGet, video["url"].(string), nil)
	req.Header.Set("Range", "bytes=0-99")
	rr, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer rr.Body.Close()
	rb, _ := io.ReadAll(rr.Body)
	if rr.StatusCode != 206 || len(rb) != 100 {
		t.Fatalf("range %d %d", rr.StatusCode, len(rb))
	}
	reps := h.userReports()
	if len(reps) != 1 || !containsArg(reps[0].Argv, "--tools", "reference_to_video") {
		t.Fatalf("%+v", reps)
	}
	if !strings.Contains(string(readCalls(t, h)[0].Input), filepathIn(h)) {
		t.Fatal(string(readCalls(t, h)[0].Input))
	}
}

// TestVideoTextOnly checks the two-tool argv and that first_frame is the image output.
func TestVideoTextOnly(t *testing.T) {
	h := start(t, startOpt{scenario: "video-t2v", media: true})
	res := postMedia(t, h, "/v1/videos/generations", `{"prompt":"A lighthouse","duration":6,"aspect_ratio":"16:9"}`)
	if res.code != 200 {
		t.Fatal(string(res.body))
	}
	id := requestID(t, res.body)
	st := waitVideo(t, h, id, 8*time.Second)
	if st["status"] != "done" {
		t.Fatal(st)
	}
	reps := h.userReports()
	if len(reps) != 1 || !containsArg(reps[0].Argv, "--tools", "image_gen,reference_to_video") {
		t.Fatalf("%+v", reps)
	}
	calls := readCalls(t, h)
	if len(calls) < 2 {
		t.Fatal(calls)
	}
	var frame struct {
		First string `json:"first_frame"`
	}
	_ = json.Unmarshal(calls[1].Input, &frame)
	if frame.First == "" || frame.First != calls[0].Output {
		t.Fatalf("frame %s output %s", frame.First, calls[0].Output)
	}
}

// TestVideoReversed fails the job when the video tool runs before the image exists.
func TestVideoReversed(t *testing.T) {
	h := start(t, startOpt{scenario: "video-reversed", media: true})
	res := postMedia(t, h, "/v1/videos/generations", `{"prompt":"A lighthouse","duration":6}`)
	id := requestID(t, res.body)
	st := waitVideo(t, h, id, 8*time.Second)
	if st["status"] != "failed" {
		t.Fatal(st)
	}
	errObj := st["error"].(map[string]any)
	if errObj["code"] != "internal_error" {
		t.Fatal(st)
	}
}

// TestVideoBusyAndTimeout fills the queue and fails a stuck player at the video timeout.
func TestVideoBusyAndTimeout(t *testing.T) {
	h := start(t, startOpt{scenario: "media-hang", media: true, maxVideo: 1, videoTimeout: 2 * time.Second})
	first := postMedia(t, h, "/v1/videos/generations", `{"prompt":"stuck","duration":6}`)
	if first.code != 200 {
		t.Fatal(string(first.body))
	}
	second := postMedia(t, h, "/v1/videos/generations", `{"prompt":"stuck","duration":6}`)
	if second.code != 429 || second.header.Get("Retry-After") != "5" {
		t.Fatalf("%d %s %s", second.code, second.header.Get("Retry-After"), second.body)
	}
	assertCode(t, second.code, second.body, 429, "agent_mock_busy", "")
	st := waitVideo(t, h, requestID(t, first.body), 15*time.Second)
	if st["status"] != "failed" {
		t.Fatal(st)
	}
	if st["error"].(map[string]any)["code"] != "service_unavailable" {
		t.Fatal(st)
	}
	rep := h.userReports()
	if len(rep) == 0 || procAlive(rep[0].PID) {
		t.Fatal("stuck player still running")
	}
}

// TestVideoErrors maps moderation and tier fixtures onto xAI error codes.
func TestVideoErrors(t *testing.T) {
	h := start(t, startOpt{scenario: "media-blocked", media: true})
	res := postMedia(t, h, "/v1/videos/generations", `{"prompt":"nope","duration":6}`)
	st := waitVideo(t, h, requestID(t, res.body), 8*time.Second)
	if st["error"].(map[string]any)["code"] != "invalid_argument" {
		t.Fatal(st)
	}
	h = start(t, startOpt{scenario: "media-tier", media: true})
	res = postMedia(t, h, "/v1/videos/generations", `{"prompt":"nope","duration":6}`)
	st = waitVideo(t, h, requestID(t, res.body), 8*time.Second)
	errObj := st["error"].(map[string]any)
	if errObj["code"] != "permission_denied" || !strings.Contains(errObj["message"].(string), "upgrade") {
		t.Fatal(st)
	}
}

// requestID reads request_id from a POST body.
func requestID(t *testing.T, body []byte) string {
	t.Helper()
	var ack struct {
		ID string `json:"request_id"`
	}
	if json.Unmarshal(body, &ack) != nil || ack.ID == "" {
		t.Fatal(string(body))
	}
	return ack.ID
}

// waitVideo polls until done or failed, or the deadline passes.
func waitVideo(t *testing.T, h *harness, id string, d time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(d)
	var done map[string]any
	for time.Now().Before(deadline) {
		gr, err := http.Get(h.ts.URL + "/v1/videos/" + id)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(gr.Body)
		gr.Body.Close()
		_ = json.Unmarshal(b, &done)
		if done["status"] == "done" || done["status"] == "failed" {
			return done
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal(done)
	return nil
}

// TestVideoJPEGThumbnailAspect posts a JPEG whose APP1 holds a 10x10 SOF and
// whose real frame is 1920x1080 past 64 KiB, with no aspect_ratio. The
// reference_to_video call must ask for 16:9. A parser that returns the
// thumbnail size asks for 1:1.
func TestVideoJPEGThumbnailAspect(t *testing.T) {
	h := start(t, startOpt{scenario: "video", media: true, videoTimeout: time.Minute})
	body := `{"prompt":"pan","image":{"url":"data:image/jpeg;base64,` + base64.StdEncoding.EncodeToString(thumbnailSOFJPEG()) + `"},"duration":6}`
	res := postMedia(t, h, "/v1/videos/generations", body)
	if res.code != 200 {
		t.Fatal(string(res.body))
	}
	deadline := time.Now().Add(8 * time.Second)
	var calls []callLog
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(filepath.Join(h.dir, "calls.jsonl"))
		if err == nil && len(bytes.TrimSpace(b)) > 0 {
			calls = readCalls(t, h)
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(calls) == 0 {
		t.Fatal("no tool call")
	}
	for _, c := range calls {
		if c.Name != "reference_to_video" {
			continue
		}
		var in struct {
			Aspect string `json:"aspect_ratio"`
		}
		if json.Unmarshal(c.Input, &in) != nil || in.Aspect != "16:9" {
			t.Fatalf("aspect %q input %s", in.Aspect, c.Input)
		}
		return
	}
	t.Fatalf("no reference_to_video call in %s", calls)
}

// thumbnailSOFJPEG matches internal/media thumbnailSOFJPEG: a 65535-byte APP1
// begins with a 10x10 SOF0, and the real 1920x1080 SOF0 starts at offset 65539.
func thumbnailSOFJPEG() []byte {
	const appLen = 65535
	payload := appLen - 2
	decoy := []byte{0xff, 0xc0, 0x00, 0x0b, 0x08, 0x00, 0x0a, 0x00, 0x0a, 0x01, 0x01, 0x11, 0x00}
	buf := make([]byte, 0, 2+4+payload+13+2)
	buf = append(buf, 0xff, 0xd8)
	buf = append(buf, 0xff, 0xe1)
	buf = append(buf, byte(appLen>>8), byte(appLen&0xff))
	buf = append(buf, decoy...)
	buf = append(buf, make([]byte, payload-len(decoy))...)
	buf = append(buf, 0xff, 0xc0, 0x00, 0x0b, 0x08, 0x04, 0x38, 0x07, 0x80, 0x01, 0x01, 0x11, 0x00)
	buf = append(buf, 0xff, 0xd9)
	return buf
}

// b64PNG is the tiny png as base64.
func b64PNG(t *testing.T) string {
	t.Helper()
	return base64.StdEncoding.EncodeToString(tinyPNG(t))
}

// filepathIn is the media cwd input directory.
func filepathIn(h *harness) string {
	return h.srv.Runner.RootDir() + "/mcwd/in"
}
