// Package xai parses the video generation request this server accepts.
// The shape follows the xAI video API: a prompt, an optional first frame,
// reference images, keyframes, and preset voices. It does not call api.x.ai.
package xai

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/shaoboli/agent-mock/internal/grok"
	"github.com/shaoboli/agent-mock/internal/openai"
)

// DefaultModel is echoed when the request omits model.
const DefaultModel = "grok-imagine-video"

// AnimatePrompt is the tool prompt when an image is set and prompt is omitted.
const AnimatePrompt = "Animate this image with natural, subtle motion."

// Frame is one keyframe. URL is staged like any other image. Timestamp is seconds.
type Frame struct {
	// URL is the image URL or data URI.
	URL string
	// Timestamp is timestamp_s. It must be greater than 0 and less than duration.
	Timestamp float64
}

// Request is a validated video generation. Empty Aspect means the server
// should pick the closest ratio from a png or jpeg first frame, else 16:9.
type Request struct {
	// Model is echoed on the status. Empty was replaced with DefaultModel.
	Model string
	// Prompt is the tool prompt. It may be the animate default.
	Prompt string
	// Duration is 1..15. Omitted means 8.
	Duration int
	// Aspect is one of the seven ratios, or empty when the caller omitted it.
	Aspect string
	// Resolution is 480p or 720p. 1080p has already been rewritten to 720p.
	Resolution string
	// Ignored lists accepted-but-unused fields, including resolution when 1080p.
	Ignored []string
	// First is image.url. Empty when the caller did not send a first frame.
	First string
	// Refs are reference image URLs, at most 4.
	Refs []string
	// Frames are keyframes, at most 4.
	Frames []Frame
	// Voices are voice ids, at most 3. A url form was rejected.
	Voices []string
	// TextToVideo is true when there is no image, reference, keyframe, or voice.
	TextToVideo bool
}

// videoRatios are the aspect_ratio values reference_to_video accepts.
var videoRatios = map[string]bool{
	"1:1": true, "16:9": true, "9:16": true, "4:3": true, "3:4": true, "3:2": true, "2:3": true,
}

// Parse reads a JSON video body. A bad field is *openai.RequestError with
// unsupported_parameter. reference_audios that use url are rejected.
// Prompt may be omitted only when image is set; the default animate sentence is used.
func Parse(body []byte) (Request, error) {
	var raw map[string]json.RawMessage
	if json.Unmarshal(body, &raw) != nil {
		return Request{}, openaiBad("invalid_json", "request body is not a JSON object")
	}
	var out Request
	out.Model = str(raw, "model")
	if out.Model == "" {
		out.Model = DefaultModel
	}
	prompt, err := promptOf(raw)
	if err != nil {
		return Request{}, err
	}
	out.Prompt = prompt
	dur, err := durationOf(raw)
	if err != nil {
		return Request{}, err
	}
	out.Duration = dur
	aspect := str(raw, "aspect_ratio")
	if aspect != "" && !videoRatios[aspect] {
		return Request{}, openaiBad("unsupported_parameter", `parameter "aspect_ratio" value "`+aspect+`" is unsupported`)
	}
	out.Aspect = aspect
	res, ignoredRes, err := resolutionOf(raw)
	if err != nil {
		return Request{}, err
	}
	out.Resolution = res
	if ignoredRes {
		out.Ignored = append(out.Ignored, "resolution")
	}
	for _, name := range []string{"output", "storage_options", "user"} {
		if present(raw, name) {
			out.Ignored = append(out.Ignored, name)
		}
	}
	if present(raw, "model") {
		out.Ignored = append([]string{"model"}, out.Ignored...)
	}
	if v, ok := raw["image"]; ok {
		u, err := urlField(v, "image")
		if err != nil {
			return Request{}, err
		}
		out.First = u
	}
	if v, ok := raw["reference_images"]; ok {
		urls, err := urlList(v, "reference_images", 4)
		if err != nil {
			return Request{}, err
		}
		out.Refs = urls
	}
	if v, ok := raw["keyframes"]; ok {
		frames, err := framesOf(v, out.Duration)
		if err != nil {
			return Request{}, err
		}
		out.Frames = frames
	}
	if v, ok := raw["reference_audios"]; ok {
		voices, err := voicesOf(v)
		if err != nil {
			return Request{}, err
		}
		out.Voices = voices
	}
	if out.First == "" && out.Prompt == "" {
		return Request{}, openaiBad("unsupported_parameter", `parameter "prompt" is required`)
	}
	if out.First != "" && out.Prompt == "" {
		out.Prompt = AnimatePrompt
	}
	out.TextToVideo = out.First == "" && len(out.Refs) == 0 && len(out.Frames) == 0 && len(out.Voices) == 0
	return out, nil
}

// ErrorCode maps an agent-mock grok code onto the xAI video error code.
// content_policy_violation becomes invalid_argument. A missing feature or a
// logged-out grok becomes permission_denied. Rate, usage, and timeout become
// service_unavailable. Everything else, including safety failures, is internal_error.
func ErrorCode(internal string) string {
	switch internal {
	case grok.CodeContentPolicy:
		return "invalid_argument"
	case grok.CodeMediaUnavailable, grok.CodeNotLoggedIn:
		return "permission_denied"
	case grok.CodeRateLimited, grok.CodeUsageLimit, grok.CodeTimeout:
		return "service_unavailable"
	default:
		return "internal_error"
	}
}

// promptOf reads prompt. Missing is allowed here; the caller requires it when
// there is no image. Over 4000 characters is unsupported_parameter.
func promptOf(raw map[string]json.RawMessage) (string, error) {
	v, ok := raw["prompt"]
	if !ok || string(v) == "null" {
		return "", nil
	}
	var s string
	if json.Unmarshal(v, &s) != nil {
		return "", openaiBad("unsupported_parameter", `parameter "prompt" must be a string`)
	}
	if utf8.RuneCountInString(s) > 4000 {
		return "", openaiBad("unsupported_parameter", `parameter "prompt" exceeds 4000 characters`)
	}
	return s, nil
}

// durationOf reads duration. Omitted means 8. Outside 1..15 is unsupported_parameter.
func durationOf(raw map[string]json.RawMessage) (int, error) {
	v, ok := raw["duration"]
	if !ok || string(v) == "null" {
		return 8, nil
	}
	var n int
	if json.Unmarshal(v, &n) != nil || n < 1 || n > 15 {
		return 0, openaiBad("unsupported_parameter", `parameter "duration" must be from 1 to 15`)
	}
	return n, nil
}

// resolutionOf reads resolution. Empty means 480p. 1080p is returned as 720p
// and ignored is true. Any other value is unsupported_parameter.
func resolutionOf(raw map[string]json.RawMessage) (string, bool, error) {
	s := str(raw, "resolution")
	switch s {
	case "", "480p":
		return "480p", false, nil
	case "720p":
		return "720p", false, nil
	case "1080p":
		return "720p", true, nil
	default:
		return "", false, openaiBad("unsupported_parameter", `parameter "resolution" value "`+s+`" is unsupported`)
	}
}

// framesOf reads keyframes. More than 4, or a timestamp that is not strictly
// inside the duration, is unsupported_parameter.
func framesOf(raw json.RawMessage, duration int) ([]Frame, error) {
	var arr []map[string]json.RawMessage
	if json.Unmarshal(raw, &arr) != nil {
		return nil, openaiBad("unsupported_parameter", `parameter "keyframes" must be an array`)
	}
	if len(arr) > 4 {
		return nil, openaiBad("unsupported_parameter", `parameter "keyframes" accepts at most 4`)
	}
	var out []Frame
	for _, one := range arr {
		u, err := frameURL(one)
		if err != nil {
			return nil, err
		}
		var ts float64
		if json.Unmarshal(one["timestamp_s"], &ts) != nil || !(ts > 0 && ts < float64(duration)) {
			return nil, openaiBad("unsupported_parameter", `parameter "keyframes" timestamp_s must be greater than 0 and less than duration`)
		}
		out = append(out, Frame{URL: u, Timestamp: ts})
	}
	return out, nil
}

// frameURL reads image.url or url from a keyframe object.
func frameURL(one map[string]json.RawMessage) (string, error) {
	if img, ok := one["image"]; ok {
		return urlField(img, "keyframes")
	}
	if u := str(one, "url"); u != "" {
		return u, nil
	}
	return "", openaiBad("invalid_image", "keyframe image url is required")
}

// voicesOf reads reference_audios[].voice_id. A url field is unsupported_parameter.
// More than 3 voices is unsupported_parameter.
func voicesOf(raw json.RawMessage) ([]string, error) {
	var arr []map[string]json.RawMessage
	if json.Unmarshal(raw, &arr) != nil {
		return nil, openaiBad("unsupported_parameter", `parameter "reference_audios" must be an array`)
	}
	if len(arr) > 3 {
		return nil, openaiBad("unsupported_parameter", `parameter "reference_audios" accepts at most 3`)
	}
	var out []string
	for _, one := range arr {
		if present(one, "url") {
			return nil, openaiBad("unsupported_parameter", `parameter "reference_audios" url is unsupported`)
		}
		id := str(one, "voice_id")
		if id == "" {
			return nil, openaiBad("unsupported_parameter", `parameter "reference_audios" voice_id is required`)
		}
		out = append(out, id)
	}
	return out, nil
}

// urlList reads a list of {url} objects, at most maxn.
func urlList(raw json.RawMessage, field string, maxn int) ([]string, error) {
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) != nil {
		return nil, openaiBad("unsupported_parameter", `parameter "`+field+`" must be an array`)
	}
	if len(arr) > maxn {
		return nil, openaiBad("unsupported_parameter", `parameter "`+field+`" accepts at most `+itoa(maxn))
	}
	var out []string
	for _, one := range arr {
		u, err := urlField(one, field)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, nil
}

// urlField reads a string or an object with url. file_id is unsupported_parameter.
func urlField(raw json.RawMessage, field string) (string, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return "", openaiBad("invalid_image", field+" url is required")
		}
		return s, nil
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return "", openaiBad("invalid_image", field+" must be a url")
	}
	if present(obj, "file_id") {
		return "", openaiBad("unsupported_parameter", `parameter "file_id" is unsupported`)
	}
	u := str(obj, "url")
	if u == "" {
		return "", openaiBad("invalid_image", field+" url is required")
	}
	return u, nil
}

// openaiBad builds a 400 the HTTP layer already knows how to write.
func openaiBad(code, message string) error {
	return &openai.RequestError{Status: 400, Type: "invalid_request_error", Code: code, Message: message}
}

// str reads a JSON string. Missing or non-string is empty.
func str(raw map[string]json.RawMessage, key string) string {
	v, ok := raw[key]
	if !ok {
		return ""
	}
	var s string
	if json.Unmarshal(v, &s) != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

// present reports a key that was sent and is not null.
func present(raw map[string]json.RawMessage, key string) bool {
	v, ok := raw[key]
	return ok && string(v) != "null"
}

// itoa formats a small limit for an error message.
func itoa(n int) string {
	if n == 4 {
		return "4"
	}
	if n == 3 {
		return "3"
	}
	return "0"
}
