package xai

import (
	"strings"
	"testing"

	"github.com/shaoboli/agent-mock/internal/grok"
	"github.com/shaoboli/agent-mock/internal/openai"
)

// TestParseVideoDefaults checks duration, resolution, the animate prompt, and 1080p.
func TestParseVideoDefaults(t *testing.T) {
	req, err := Parse([]byte(`{"image":{"url":"https://example.com/a.png"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.Prompt != AnimatePrompt || req.Duration != 8 || req.Resolution != "480p" || req.Model != DefaultModel || req.TextToVideo {
		t.Fatalf("%+v", req)
	}
	req, err = Parse([]byte(`{"prompt":"pan <IMAGE_0>","resolution":"1080p","duration":6,"model":"grok-imagine-video-1.5"}`))
	if err != nil || req.Resolution != "720p" || req.Prompt != "pan <IMAGE_0>" || !contains(req.Ignored, "resolution") || !req.TextToVideo {
		t.Fatalf("%+v %v", req, err)
	}
	_, err = Parse([]byte(`{"prompt":"x","reference_audios":[{"url":"https://example.com/a.mp3"}]}`))
	re, _ := err.(*openai.RequestError)
	if re == nil || re.Code != "unsupported_parameter" || !strings.Contains(re.Message, "url") {
		t.Fatal(err)
	}
	_, err = Parse([]byte(`{"prompt":"x","duration":6,"keyframes":[{"image":{"url":"https://example.com/a.png"},"timestamp_s":6}]}`))
	if err == nil {
		t.Fatal("timestamp at duration")
	}
}

// TestVideoErrorCode maps internal codes onto the xAI status enum.
func TestVideoErrorCode(t *testing.T) {
	if ErrorCode(grok.CodeContentPolicy) != "invalid_argument" {
		t.Fatal("policy")
	}
	if ErrorCode(grok.CodeMediaUnavailable) != "permission_denied" || ErrorCode(grok.CodeNotLoggedIn) != "permission_denied" {
		t.Fatal("denied")
	}
	if ErrorCode(grok.CodeTimeout) != "service_unavailable" || ErrorCode(grok.CodeRateLimited) != "service_unavailable" || ErrorCode(grok.CodeUsageLimit) != "service_unavailable" {
		t.Fatal("unavailable")
	}
	if ErrorCode(grok.CodeBadOutput) != "internal_error" || ErrorCode("safety") != "internal_error" {
		t.Fatal("internal")
	}
}

// contains reports whether list has want.
func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
