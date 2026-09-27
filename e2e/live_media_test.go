//go:build live

package e2e

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// TestLiveMedia generates one image with the logged-in grok CLI.
// It runs only when AGENT_MOCK_LIVE_MEDIA=1. The unit suite does not build this file.
func TestLiveMedia(t *testing.T) {
	if os.Getenv("AGENT_MOCK_LIVE_MEDIA") != "1" {
		t.Skip("set AGENT_MOCK_LIVE_MEDIA=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	run := startServer(t, ctx, nil)
	client := openai.NewClient(option.WithBaseURL(run.base), option.WithAPIKey("dev"), option.WithMaxRetries(0))
	img, err := client.Images.Generate(ctx, openai.ImageGenerateParams{
		Prompt: "A red apple on a white table, studio photo",
		Size:   openai.ImageGenerateParamsSize1024x1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(img.Data) == 0 || img.Data[0].URL == "" {
		t.Fatal(img.Data)
	}
}
