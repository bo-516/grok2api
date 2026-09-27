package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"

	"github.com/openai/openai-go/v3"
)

// TestImageGenerateURL drives Images.Generate through fakegrok and checks the URL bytes.
func TestImageGenerateURL(t *testing.T) {
	h := start(t, startOpt{scenario: "image-gen", media: true})
	c := h.client()
	img, err := c.Images.Generate(context.Background(), openai.ImageGenerateParams{
		Prompt: "A red apple",
		Size:   openai.ImageGenerateParamsSize1792x1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(img.Data) != 1 || img.Data[0].RevisedPrompt != "A red apple" {
		t.Fatalf("%+v", img.Data)
	}
	if !regexp.MustCompile(`^http://127\.0\.0\.1:\d+/v1/media/[0-9a-f]{32}\.(png|jpg|webp)$`).MatchString(img.Data[0].URL) {
		t.Fatal(img.Data[0].URL)
	}
	res, err := http.Get(img.Data[0].URL)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	want, err := os.ReadFile(filepath.Join(h.dir, "player-1.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "image/") || !bytes.Equal(body, want) {
		t.Fatalf("status %d type %s len %d %d", res.StatusCode, res.Header.Get("Content-Type"), len(body), len(want))
	}
	rep := h.userReports()
	if len(rep) != 1 || !containsArg(rep[0].Argv, "--tools", "image_gen") || !containsArg(rep[0].Argv, "--disallowed-tools", "search_tool,use_tool") || !strings.Contains(rep[0].Body, `"aspect_ratio":"16:9"`) {
		t.Fatalf("%+v", rep)
	}
	if !strings.Contains(strings.Join(rep[0].Argv, " "), "mcwd") {
		t.Fatal(rep[0].Argv)
	}
}

// TestImageB64AndN covers b64_json, n=2 env, and n=5 not starting grok.
func TestImageB64AndN(t *testing.T) {
	h := start(t, startOpt{scenario: "image-gen", media: true})
	c := h.client()
	img, err := c.Images.Generate(context.Background(), openai.ImageGenerateParams{
		Prompt:         "A red apple",
		Size:           openai.ImageGenerateParamsSize1792x1024,
		ResponseFormat: openai.ImageGenerateParamsResponseFormatB64JSON,
	})
	if err != nil || img.Data[0].URL != "" {
		t.Fatal(err, img.Data)
	}
	got, err := base64.StdEncoding.DecodeString(img.Data[0].B64JSON)
	want, _ := os.ReadFile(filepath.Join(h.dir, "player-1.jpg"))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("b64 %v %d %d", err, len(got), len(want))
	}

	h2 := start(t, startOpt{scenario: "image-gen", media: true})
	c2 := h2.client()
	many, err := c2.Images.Generate(context.Background(), openai.ImageGenerateParams{
		Prompt: "A red apple",
		N:      openai.Int(2),
		Size:   openai.ImageGenerateParamsSize1024x1024,
	})
	if err != nil || len(many.Data) != 2 || many.Data[0].URL == many.Data[1].URL {
		t.Fatal(err, many.Data)
	}
	reps := h2.userReports()
	if len(reps) != 1 || reps[0].ImageParallel != "2" {
		t.Fatalf("%+v", reps)
	}

	h3 := start(t, startOpt{scenario: "image-gen", media: true})
	bad := postMedia(t, h3, "/v1/images/generations", `{"prompt":"A red apple","n":5}`)
	assertCode(t, bad.code, bad.body, 400, "unsupported_parameter", "n")
	if len(h3.userReports()) != 0 {
		t.Fatal("n=5 started grok")
	}
}

// TestImageEditStages uploads one PNG and checks the tool path and cleanup.
func TestImageEditStages(t *testing.T) {
	h := start(t, startOpt{scenario: "image-edit", media: true})
	png := tinyPNG(t)
	c := h.client()
	if _, err := c.Images.Edit(context.Background(), openai.ImageEditParams{
		Image:  openai.ImageEditParamsImageUnion{OfFile: openai.File(bytes.NewReader(png), "a.png", "image/png")},
		Prompt: "sketch",
	}); err != nil {
		t.Fatal(err)
	}
	calls := readCalls(t, h)
	root := filepath.Join(h.srv.Runner.RootDir(), "mcwd", "in")
	if len(calls) == 0 || !strings.Contains(string(calls[0].Input), root) {
		t.Fatalf("%s", calls)
	}
	entries, err := os.ReadDir(root)
	if err == nil && len(entries) != 0 {
		t.Fatalf("staged dir remains: %v", entries)
	}
	c2 := h.client()
	_, err = c2.Images.Edit(context.Background(), openai.ImageEditParams{
		Image:  openai.ImageEditParamsImageUnion{OfFile: openai.File(bytes.NewReader(png), "a.png", "image/png")},
		Prompt: "sketch",
		Mask:   bytes.NewReader(png),
	})
	sdkFail(t, err, 400, "unsupported_parameter", "mask")
}

// TestImageEditSources rejects http and loopback https and reuses a media URL with no outbound call.
func TestImageEditSources(t *testing.T) {
	h := start(t, startOpt{scenario: "image-gen,image-edit", media: true})
	c := h.client()
	img, err := c.Images.Generate(context.Background(), openai.ImageGenerateParams{Prompt: "A red apple", Size: openai.ImageGenerateParamsSize1024x1024})
	if err != nil {
		t.Fatal(err)
	}
	h.srv.Stager.Transport = roundTrip(func(*http.Request) (*http.Response, error) {
		t.Fatal("media URL was fetched")
		return nil, context.Canceled
	})
	ok := postMedia(t, h, "/v1/images/edits", `{"prompt":"sketch","image":{"url":"`+img.Data[0].URL+`"}}`)
	if ok.code != 200 {
		t.Fatal(string(ok.body))
	}
	httpURL := postMedia(t, h, "/v1/images/edits", `{"prompt":"sketch","image":{"url":"http://10.0.0.1/x.png"}}`)
	assertCode(t, httpURL.code, httpURL.body, 400, "invalid_image", "")
	loop := postMedia(t, h, "/v1/images/edits", `{"prompt":"sketch","image":{"url":"https://127.0.0.1:9/x.png"}}`)
	assertCode(t, loop.code, loop.body, 400, "invalid_image", "")
}

// TestMediaGuard is the fakegrok kill and bad-output matrix.
func TestMediaGuard(t *testing.T) {
	png := tinyPNG(t)
	h := start(t, startOpt{scenario: "media-evil-path", media: true})
	before := h.srv.Store.Count()
	c := h.client()
	_, err := c.Images.Edit(context.Background(), openai.ImageEditParams{
		Image:  openai.ImageEditParamsImageUnion{OfFile: openai.File(bytes.NewReader(png), "a.png", "image/png")},
		Prompt: "sketch",
	})
	sdkFail(t, err, 500, "unsafe_grok_tool_input", "")
	if h.srv.Store.Count() != before {
		t.Fatal("store changed")
	}
	rep := h.userReports()
	if len(rep) == 0 || procAlive(rep[0].PID) {
		t.Fatalf("pid alive %+v", rep)
	}

	extra := start(t, startOpt{scenario: "media-extra-tools", media: true})
	c = extra.client()
	_, err = c.Images.Edit(context.Background(), openai.ImageEditParams{
		Image:  openai.ImageEditParamsImageUnion{OfFile: openai.File(bytes.NewReader(png), "a.png", "image/png")},
		Prompt: "sketch",
	})
	sdkFail(t, err, 500, "unsafe_grok_toolset", "")

	empty := start(t, startOpt{scenario: "media-empty-tools", media: true})
	c = empty.client()
	_, err = c.Images.Edit(context.Background(), openai.ImageEditParams{
		Image:  openai.ImageEditParamsImageUnion{OfFile: openai.File(bytes.NewReader(png), "a.png", "image/png")},
		Prompt: "sketch",
	})
	sdkFail(t, err, 403, "grok_media_unavailable", "")

	for _, sc := range []string{"media-bad-outside", "media-bad-symlink", "media-bad-text"} {
		h := start(t, startOpt{scenario: sc, media: true})
		n := h.srv.Store.Count()
		c := h.client()
		_, err := c.Images.Edit(context.Background(), openai.ImageEditParams{
			Image:  openai.ImageEditParamsImageUnion{OfFile: openai.File(bytes.NewReader(png), "a.png", "image/png")},
			Prompt: "sketch",
		})
		sdkFail(t, err, 502, "grok_bad_output", "")
		if h.srv.Store.Count() != n {
			t.Fatalf("%s stored a file", sc)
		}
	}

	in := start(t, startOpt{scenario: "media-inline", media: true})
	c = in.client()
	if _, err := c.Images.Edit(context.Background(), openai.ImageEditParams{
		Image:  openai.ImageEditParamsImageUnion{OfFile: openai.File(bytes.NewReader(png), "a.png", "image/png")},
		Prompt: "sketch",
	}); err != nil {
		t.Fatal(err)
	}
	block := start(t, startOpt{scenario: "media-blocked", media: true})
	c = block.client()
	_, err = c.Images.Edit(context.Background(), openai.ImageEditParams{
		Image:  openai.ImageEditParamsImageUnion{OfFile: openai.File(bytes.NewReader(png), "a.png", "image/png")},
		Prompt: "sketch",
	})
	sdkFail(t, err, 400, "content_policy_violation", "")
	tier := start(t, startOpt{scenario: "media-tier", media: true})
	c = tier.client()
	_, err = c.Images.Edit(context.Background(), openai.ImageEditParams{
		Image:  openai.ImageEditParamsImageUnion{OfFile: openai.File(bytes.NewReader(png), "a.png", "image/png")},
		Prompt: "sketch",
	})
	sdkFail(t, err, 403, "grok_media_unavailable", "upgrade")
}

// TestChatRejectsImageTool keeps a chat run at 500 when init offers image_gen.
func TestChatRejectsImageTool(t *testing.T) {
	h := start(t, startOpt{scenario: "init-image-gen"})
	res, err := http.Post(h.ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"superllm","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	assertCode(t, res.StatusCode, body, 500, "unsafe_grok_toolset", "")
}

// TestMediaDisabledAndHealth covers -media=false, healthz, the access line, and /doc.
func TestMediaDisabledAndHealth(t *testing.T) {
	off := start(t, startOpt{})
	for _, path := range []string{"/v1/images/generations", "/v1/images/edits", "/v1/videos/generations"} {
		res := postMedia(t, off, path, `{"prompt":"x"}`)
		assertCode(t, res.code, res.body, 404, "media_disabled", "")
	}
	for _, path := range []string{"/v1/videos/missing", "/v1/media/0123456789abcdef0123456789abcdef.png"} {
		res, err := http.Get(off.ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		assertCode(t, res.StatusCode, body, 404, "media_disabled", "")
	}
	h := start(t, startOpt{scenario: "image-gen", media: true})
	res := postMedia(t, h, "/v1/images/generations", `{"prompt":"unique-apple-prompt","size":"1024x1024"}`)
	if res.code != 200 {
		t.Fatal(string(res.body))
	}
	log := h.log.String()
	if strings.Contains(log, "unique-apple-prompt") {
		t.Fatal(log)
	}
	for _, s := range []string{"kind=image", "tools=image_gen", "n=1", "status=200", "dur=", "in=", "out=", "files=1"} {
		if !strings.Contains(log, s) {
			t.Fatalf("missing %s in %s", s, log)
		}
	}
	hr, err := http.Get(h.ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer hr.Body.Close()
	hb, _ := io.ReadAll(hr.Body)
	for _, s := range []string{`"media"`, `"media_tools"`, `"video_jobs"`} {
		if !strings.Contains(string(hb), s) {
			t.Fatal(string(hb))
		}
	}
	doc, err := http.Get(h.ts.URL + "/doc")
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Body.Close()
	db, _ := io.ReadAll(doc.Body)
	for _, s := range []string{"/v1/images/generations", "/v1/images/edits", "/v1/videos/generations", "1 到 4", "20 MiB"} {
		if !strings.Contains(string(db), s) {
			t.Fatalf("missing %s\n%s", s, db)
		}
	}
	badName, err := http.Get(h.ts.URL + "/v1/media/not-a-file")
	if err != nil {
		t.Fatal(err)
	}
	badName.Body.Close()
	if badName.StatusCode != 404 {
		t.Fatal(badName.StatusCode)
	}
}

type rawResp struct {
	code   int
	body   []byte
	header http.Header
}

// postMedia posts a JSON body to path.
func postMedia(t *testing.T, h *harness, path, body string) rawResp {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return rawResp{res.StatusCode, b, res.Header}
}

// sdkFail requires err to carry the HTTP status, error code, and optional message text.
func sdkFail(t *testing.T, err error, status int, code, field string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error")
	}
	s := err.Error()
	if !strings.Contains(s, code) || (field != "" && !strings.Contains(s, field)) {
		t.Fatalf("status %d code %s field %s err %s", status, code, field, s)
	}
	if !strings.Contains(s, " "+itoa(status)+" ") && !strings.Contains(s, ":"+itoa(status)) && !strings.Contains(s, itoa(status)+" ") {
		// The SDK text is `POST "...": 500 Internal Server Error`.
		if !strings.Contains(s, itoa(status)) {
			t.Fatal(s)
		}
	}
}

// itoa formats a small status code.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// tinyPNG is a 2x2 png.
func tinyPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewNRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// procAlive reports whether pid is still running.
func procAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

type callLog struct {
	Name   string          `json:"name"`
	Input  json.RawMessage `json:"input"`
	Output string          `json:"output"`
}

// readCalls reads FAKEGROK_CALLS lines that include an output path.
func readCalls(t *testing.T, h *harness) []callLog {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(h.dir, "calls.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out []callLog
	for _, line := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var c callLog
		if json.Unmarshal(line, &c) != nil {
			t.Fatal(string(line))
		}
		out = append(out, c)
	}
	return out
}

// roundTrip is a test transport.
type roundTrip func(*http.Request) (*http.Response, error)

// RoundTrip calls the function.
func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
