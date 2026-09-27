package openai

import (
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf8"
)

// maxPrompt is the longest image or video prompt, in Unicode characters.
const maxPrompt = 4000

// ImageGen is a validated POST /v1/images/generations body.
// Aspect is the value to pass to image_gen. Ignored lists accepted-but-unused fields.
type ImageGen struct {
	// Prompt is the tool prompt. It is non-empty and at most 4000 characters.
	Prompt string
	// N is how many images to return, from 1 to 4. Omitted means 1.
	N int
	// Aspect is the mapped image_gen aspect_ratio, including auto.
	Aspect string
	// Format is url or b64_json. Empty was defaulted to url.
	Format string
	// Ignored is the X-Agent-Mock-Ignored list, in a stable order.
	Ignored []string
}

// EditSource is one edit input before staging. Body is a multipart file.
// URL is a data URI or http(s) URL. Exactly one of them is set.
type EditSource struct {
	// Body is the uploaded file. Nil when URL is set.
	Body []byte
	// Name is the multipart filename. It is not used as the staged name.
	Name string
	// URL is a data URI or an http(s) URL.
	URL string
}

// ImageEdit is a validated edit. PassAspect is false for a single image so a
// size or aspect_ratio is listed in Ignored and not sent to image_edit.
type ImageEdit struct {
	// Prompt is the tool prompt.
	Prompt string
	// N is how many edited images to return, from 1 to 4.
	N int
	// Aspect is the mapped ratio when PassAspect is true. It may be empty.
	Aspect string
	// Format is url or b64_json.
	Format string
	// Ignored lists fields that were accepted and not applied.
	Ignored []string
	// Sources are the input images, in order, at most 5.
	Sources []EditSource
	// PassAspect is true only for a multi-image edit that set size or aspect_ratio.
	PassAspect bool
}

// ParseImageGen reads a JSON generations body.
// An empty prompt, a bad n, stream, partial_images, or both size and aspect_ratio
// return 400 unsupported_parameter. The returned Aspect is ready for image_gen.
func ParseImageGen(body []byte) (ImageGen, error) {
	var raw map[string]json.RawMessage
	if json.Unmarshal(body, &raw) != nil {
		return ImageGen{}, bad("invalid_json", "request body is not a JSON object")
	}
	if err := rejectMediaFlags(raw); err != nil {
		return ImageGen{}, err
	}
	prompt, err := needPrompt(raw, true)
	if err != nil {
		return ImageGen{}, err
	}
	n, err := needN(raw)
	if err != nil {
		return ImageGen{}, err
	}
	aspect, approx, err := mapAspect(str(raw, "size"), str(raw, "aspect_ratio"))
	if err != nil {
		return ImageGen{}, err
	}
	format, err := needFormat(raw)
	if err != nil {
		return ImageGen{}, err
	}
	ignored := imageIgnored(raw)
	if approx {
		ignored = append(ignored, "aspect_ratio")
	}
	return ImageGen{Prompt: prompt, N: n, Aspect: aspect, Format: format, Ignored: ignored}, nil
}

// ParseImageEditJSON reads an xAI-style JSON edit.
// image.url and images[].url are both accepted and concatenated, image first.
// mask and file_id are 400. More than 5 images is 400 invalid_image.
func ParseImageEditJSON(body []byte) (ImageEdit, error) {
	var raw map[string]json.RawMessage
	if json.Unmarshal(body, &raw) != nil {
		return ImageEdit{}, bad("invalid_json", "request body is not a JSON object")
	}
	if err := rejectEditFlags(raw); err != nil {
		return ImageEdit{}, err
	}
	prompt, err := needPrompt(raw, true)
	if err != nil {
		return ImageEdit{}, err
	}
	n, err := needN(raw)
	if err != nil {
		return ImageEdit{}, err
	}
	var sources []EditSource
	if v, ok := raw["image"]; ok {
		sources, err = appendURL(sources, v, "image")
		if err != nil {
			return ImageEdit{}, err
		}
	}
	if v, ok := raw["images"]; ok {
		var arr []json.RawMessage
		if json.Unmarshal(v, &arr) != nil {
			return ImageEdit{}, bad("unsupported_parameter", `parameter "images" must be an array`)
		}
		for _, one := range arr {
			sources, err = appendURL(sources, one, "images")
			if err != nil {
				return ImageEdit{}, err
			}
		}
	}
	if len(sources) == 0 {
		return ImageEdit{}, bad("invalid_image", "image is required")
	}
	if len(sources) > 5 {
		return ImageEdit{}, bad("invalid_image", "at most 5 images are accepted")
	}
	return finishEdit(raw, prompt, n, sources)
}

// finishEdit applies size and aspect rules shared by JSON and multipart edits.
// A single image does not pass aspect_ratio to the tool. A bad size is still 400.
func finishEdit(raw map[string]json.RawMessage, prompt string, n int, sources []EditSource) (ImageEdit, error) {
	size, aspect := str(raw, "size"), str(raw, "aspect_ratio")
	format, err := needFormat(raw)
	if err != nil {
		return ImageEdit{}, err
	}
	ignored := imageIgnored(raw)
	pass := false
	mapped := ""
	if size != "" || aspect != "" {
		ratio, approx, err := mapAspect(size, aspect)
		if err != nil {
			return ImageEdit{}, err
		}
		if len(sources) == 1 {
			if size != "" {
				ignored = append(ignored, "size")
			}
			if aspect != "" {
				ignored = append(ignored, "aspect_ratio")
			}
		} else {
			pass = true
			mapped = ratio
			if approx {
				ignored = append(ignored, "aspect_ratio")
			}
		}
	}
	return ImageEdit{Prompt: prompt, N: n, Aspect: mapped, Format: format, Ignored: ignored, Sources: sources, PassAspect: pass}, nil
}

// rejectMediaFlags rejects stream:true and any partial_images on a generations body.
func rejectMediaFlags(raw map[string]json.RawMessage) error {
	if b, ok := raw["stream"]; ok {
		var v bool
		var s string
		if (json.Unmarshal(b, &v) == nil && v) || (json.Unmarshal(b, &s) == nil && s == "true") {
			return bad("unsupported_parameter", `parameter "stream" is unsupported`)
		}
	}
	if _, ok := raw["partial_images"]; ok {
		return bad("unsupported_parameter", `parameter "partial_images" is unsupported`)
	}
	return nil
}

// rejectEditFlags rejects mask, file_id, stream, and partial_images.
// mask says image_edit has no mask. file_id is not a supported image source.
func rejectEditFlags(raw map[string]json.RawMessage) error {
	if err := rejectMediaFlags(raw); err != nil {
		return err
	}
	if _, ok := raw["mask"]; ok {
		return bad("unsupported_parameter", `parameter "mask" is unsupported: image_edit has no mask`)
	}
	if _, ok := raw["file_id"]; ok {
		return bad("unsupported_parameter", `parameter "file_id" is unsupported`)
	}
	return nil
}

// needPrompt reads prompt. required false allows an empty value (video with an image).
// Over 4000 characters is unsupported_parameter. A non-string is too.
func needPrompt(raw map[string]json.RawMessage, required bool) (string, error) {
	v, ok := raw["prompt"]
	if !ok || string(v) == "null" {
		if required {
			return "", bad("unsupported_parameter", `parameter "prompt" is required`)
		}
		return "", nil
	}
	var s string
	if json.Unmarshal(v, &s) != nil {
		return "", bad("unsupported_parameter", `parameter "prompt" must be a string`)
	}
	if required && strings.TrimSpace(s) == "" {
		return "", bad("unsupported_parameter", `parameter "prompt" is required`)
	}
	if utf8.RuneCountInString(s) > maxPrompt {
		return "", bad("unsupported_parameter", `parameter "prompt" exceeds 4000 characters`)
	}
	return s, nil
}

// needN reads n. Omitted means 1. Any value outside 1..4 is unsupported_parameter
// and the message names n. A non-integer is the same error.
func needN(raw map[string]json.RawMessage) (int, error) {
	v, ok := raw["n"]
	if !ok || string(v) == "null" {
		return 1, nil
	}
	n, ok := jsonInt(v)
	if !ok || n < 1 || n > 4 {
		return 0, bad("unsupported_parameter", `parameter "n" must be from 1 to 4`)
	}
	return n, nil
}

// jsonInt reads a JSON number or a numeric string. A bool or object is not ok.
func jsonInt(v json.RawMessage) (int, bool) {
	var n int
	if json.Unmarshal(v, &n) == nil {
		return n, true
	}
	var s string
	if json.Unmarshal(v, &s) != nil {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	return n, err == nil
}

// needFormat reads response_format. Empty means url. Anything else but b64_json is 400.
func needFormat(raw map[string]json.RawMessage) (string, error) {
	s := str(raw, "response_format")
	if s == "" {
		return "url", nil
	}
	if s != "url" && s != "b64_json" {
		return "", bad("unsupported_parameter", `parameter "response_format" is unsupported`)
	}
	return s, nil
}

// imageIgnored lists generation fields that are accepted and not applied.
// The order is stable so tests can look for a name without caring about extras.
func imageIgnored(raw map[string]json.RawMessage) []string {
	var out []string
	for _, name := range []string{"model", "quality", "style", "background", "output_compression", "resolution", "user"} {
		if present(raw, name) {
			out = append(out, name)
		}
	}
	return out
}

// appendURL adds one {url} object or a bare URL string. file_id inside the object is 400.
func appendURL(dst []EditSource, raw json.RawMessage, field string) ([]EditSource, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return nil, bad("invalid_image", field+" url is required")
		}
		return append(dst, EditSource{URL: s}), nil
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return nil, bad("invalid_image", field+" must be a url")
	}
	if _, ok := obj["file_id"]; ok {
		return nil, bad("unsupported_parameter", `parameter "file_id" is unsupported`)
	}
	u := str(obj, "url")
	if u == "" {
		return nil, bad("invalid_image", field+" url is required")
	}
	return append(dst, EditSource{URL: u}), nil
}

// mapAspect applies the size and aspect_ratio table.
// Both set is 400. approx is true when an xAI ratio was rounded to a tool ratio.
// The returned ratio is what image_gen accepts, or auto.
func mapAspect(size, aspect string) (ratio string, approx bool, err error) {
	if size != "" && aspect != "" {
		return "", false, bad("unsupported_parameter", `parameters "size" and "aspect_ratio" are mutually exclusive`)
	}
	if size != "" {
		switch size {
		case "1024x1024", "512x512", "256x256":
			return "1:1", false, nil
		case "1792x1024":
			return "16:9", false, nil
		case "1024x1792":
			return "9:16", false, nil
		case "1536x1024":
			return "3:2", false, nil
		case "1024x1536":
			return "2:3", false, nil
		case "auto":
			return "auto", false, nil
		default:
			return "", false, bad("unsupported_parameter", `parameter "size" value "`+size+`" is unsupported`)
		}
	}
	if aspect == "" {
		return "auto", false, nil
	}
	switch aspect {
	case "1:1", "16:9", "9:16", "3:2", "2:3", "auto":
		return aspect, false, nil
	case "4:3":
		return "3:2", true, nil
	case "3:4":
		return "2:3", true, nil
	case "2:1", "19.5:9", "20:9", "21:9", "5:2":
		return "16:9", true, nil
	case "1:2", "9:19.5", "9:20":
		return "9:16", true, nil
	default:
		return "", false, bad("unsupported_parameter", `parameter "aspect_ratio" value "`+aspect+`" is unsupported`)
	}
}

// str reads a JSON string field. A missing or non-string value is empty.
func str(raw map[string]json.RawMessage, key string) string {
	v, ok := raw[key]
	if !ok {
		return ""
	}
	var s string
	if json.Unmarshal(v, &s) != nil {
		return ""
	}
	return s
}

// present reports whether key was sent and is not null.
func present(raw map[string]json.RawMessage, key string) bool {
	v, ok := raw[key]
	return ok && string(v) != "null"
}
