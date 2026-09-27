// Command go-media calls a local agent-mock for one image, one edit, and one video.
// Set OPENAI_BASE_URL=http://127.0.0.1:8787/v1 and OPENAI_API_KEY=dev.
// It prints the image URL, the edited image URL, and the video URL, then exits 0.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// main runs the three media calls. A video stays pending until status is done or failed.
func main() {
	base := os.Getenv("OPENAI_BASE_URL")
	key := os.Getenv("OPENAI_API_KEY")
	if base == "" || key == "" {
		fmt.Fprintln(os.Stderr, "set OPENAI_BASE_URL and OPENAI_API_KEY")
		os.Exit(1)
	}
	client := openai.NewClient(
		option.WithBaseURL(base),
		option.WithAPIKey(key),
		option.WithMaxRetries(0),
		option.WithRequestTimeout(3*time.Minute),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	img, err := client.Images.Generate(ctx, openai.ImageGenerateParams{
		Prompt: "An orange cat sitting on a windowsill, watercolor",
		Size:   openai.ImageGenerateParamsSize1792x1024,
	})
	if err != nil || len(img.Data) == 0 || img.Data[0].URL == "" {
		fail("image", err)
	}
	fmt.Println(img.Data[0].URL)

	png := tinyPNG()
	edited, err := client.Images.Edit(ctx, openai.ImageEditParams{
		Image:  openai.ImageEditParamsImageUnion{OfFile: openai.File(bytes.NewReader(png), "a.png", "image/png")},
		Prompt: "Render this as a pencil sketch",
	})
	if err != nil || len(edited.Data) == 0 || edited.Data[0].URL == "" {
		fail("edit", err)
	}
	fmt.Println(edited.Data[0].URL)

	origin := stringsTrimAPI(base)
	id := submitVideo(origin, key, img.Data[0].URL)
	fmt.Println(waitVideo(origin, id))
}

// submitVideo posts one xAI-shaped video job and returns request_id.
// A non-200 response exits. The image URL is the first frame.
func submitVideo(origin, key, imageURL string) string {
	body, _ := json.Marshal(map[string]any{
		"prompt":   "The camera slowly pushes in",
		"image":    map[string]string{"url": imageURL},
		"duration": 6,
	})
	req, err := http.NewRequest(http.MethodPost, origin+"/v1/videos/generations", bytes.NewReader(body))
	if err != nil {
		fail("video", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		fail("video", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		fail("video", fmt.Errorf("%s", raw))
	}
	var ack struct {
		ID string `json:"request_id"`
	}
	if json.Unmarshal(raw, &ack) != nil || ack.ID == "" {
		fail("video", fmt.Errorf("%s", raw))
	}
	return ack.ID
}

// waitVideo polls every 5 seconds until the job is done or failed.
// It prints nothing and returns the video URL on done. A failure exits.
func waitVideo(origin, id string) string {
	deadline := time.Now().Add(12 * time.Minute)
	for time.Now().Before(deadline) {
		res, err := http.Get(origin + "/v1/videos/" + id)
		if err != nil {
			fail("video poll", err)
		}
		raw, _ := io.ReadAll(res.Body)
		res.Body.Close()
		var st struct {
			Status string `json:"status"`
			Video  struct {
				URL string `json:"url"`
			} `json:"video"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &st)
		if st.Status == "done" && st.Video.URL != "" {
			return st.Video.URL
		}
		if st.Status == "failed" {
			fail("video", fmt.Errorf("%s", st.Error.Message))
		}
		time.Sleep(5 * time.Second)
	}
	fail("video", fmt.Errorf("timed out"))
	return ""
}

// stringsTrimAPI removes a trailing /v1 so video URLs use the server origin.
func stringsTrimAPI(base string) string {
	if len(base) >= 3 && base[len(base)-3:] == "/v1" {
		return base[:len(base)-3]
	}
	return base
}

// tinyPNG is a 1x1 PNG so the edit example has a file to upload.
func tinyPNG() []byte {
	return []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x02, 0x00, 0x00, 0x00, 0x90, 0x77, 0x53,
		0xde, 0x00, 0x00, 0x00, 0x0c, 0x49, 0x44, 0x41, 0x54, 0x08, 0xd7, 0x63, 0xf8, 0xcf, 0xc0, 0x00,
		0x00, 0x00, 0x03, 0x00, 0x01, 0x00, 0x05, 0xfe, 0x02, 0xfe, 0xdc, 0xcc, 0x59, 0xe7, 0x00, 0x00,
		0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82,
	}
}

// fail prints the step and exits 1. A nil err still exits, because the caller
// only uses it when the reply was empty.
func fail(step string, err error) {
	if err == nil {
		err = fmt.Errorf("empty result")
	}
	fmt.Fprintf(os.Stderr, "%s: %v\n", step, err)
	os.Exit(1)
}
