package media

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/shaoboli/agent-mock/internal/grok"
)

// matchPaths compares a string or a list of path strings.
// Every actual path is checked before the lengths are compared, so an extra
// /etc/hosts next to a staged file is unsafe_grok_tool_input rather than a
// count mismatch. A $OUTPUT_<n> placeholder is left for onePath: that token
// means the image is not produced yet, which is grok_bad_output, not an
// outside path. A planned nil means the model sent a path the plan did not
// ask for. A non-string actual element is unsafe_grok_tool_input.
func (g *Guard) matchPaths(planned, actual any) error {
	ps, perr := asStrings(planned)
	as, aerr := asStrings(actual)
	if aerr != nil {
		return &grok.Error{Code: grok.CodeUnsafeInput, Message: "grok tool path was not a string. The process was killed."}
	}
	if planned != nil && perr != nil {
		return &grok.Error{Code: grok.CodeBadOutput, Message: "planned path was not a string"}
	}
	for _, got := range as {
		if outputPlaceholder(got) {
			continue
		}
		if !g.known(got) {
			return unsafePath(got)
		}
	}
	if planned != nil && len(ps) != len(as) {
		return &grok.Error{Code: grok.CodeBadOutput, Message: "grok sent the wrong number of image paths"}
	}
	for i, got := range as {
		want := ""
		if i < len(ps) {
			want = ps[i]
		}
		if err := g.onePath(want, got); err != nil {
			return err
		}
	}
	return nil
}

// matchFrames compares keyframe lists. Each image path is checked before the
// list lengths or timestamps, so a foreign path is unsafe_grok_tool_input even
// when the model also sent the wrong number of frames or a non-object item.
// timestamp_s must match numerically. A different length, after the paths are
// legal, is grok_bad_output. A non-string image is unsafe_grok_tool_input.
func (g *Guard) matchFrames(planned, actual any) error {
	al, ok2 := actual.([]any)
	if !ok2 {
		return &grok.Error{Code: grok.CodeBadOutput, Message: "grok keyframes did not match the plan"}
	}
	// shape is a count or item-shape mismatch deferred until every image path
	// has been checked. An illegal path in a later frame must win over it.
	var shape error
	for _, item := range al {
		am, isMap := item.(map[string]any)
		if !isMap || am == nil {
			if shape == nil {
				shape = &grok.Error{Code: grok.CodeBadOutput, Message: "grok keyframes did not match the plan"}
			}
			continue
		}
		got, isStr := am["image"].(string)
		if !isStr {
			return &grok.Error{Code: grok.CodeUnsafeInput, Message: "grok tool path was not a string. The process was killed."}
		}
		if outputPlaceholder(got) {
			continue
		}
		if !g.known(got) {
			return unsafePath(got)
		}
	}
	if shape != nil {
		return shape
	}
	pl, ok1 := planned.([]any)
	if !ok1 || len(pl) != len(al) {
		return &grok.Error{Code: grok.CodeBadOutput, Message: "grok keyframes did not match the plan"}
	}
	for i := range pl {
		pm, _ := pl[i].(map[string]any)
		am, _ := al[i].(map[string]any)
		if pm == nil || am == nil {
			return &grok.Error{Code: grok.CodeBadOutput, Message: "grok keyframes did not match the plan"}
		}
		want, _ := pm["image"].(string)
		got, _ := am["image"].(string)
		if err := g.onePath(want, got); err != nil {
			return err
		}
		if !sameJSON(pm["timestamp_s"], am["timestamp_s"]) {
			return &grok.Error{Code: grok.CodeBadOutput, Message: "grok keyframe timestamp did not match the plan"}
		}
	}
	return nil
}

// onePath checks one path argument. want may be a staged path or $OUTPUT_k.
// An unknown path is unsafe_grok_tool_input. A $OUTPUT_k that is not ready,
// or a different produced file, is grok_bad_output. An empty want accepts any
// path the pre-scan already classified as known.
func (g *Guard) onePath(want, got string) error {
	if strings.HasPrefix(want, "$OUTPUT_") {
		n, err := strconv.Atoi(strings.TrimPrefix(want, "$OUTPUT_"))
		if err != nil {
			return &grok.Error{Code: grok.CodeBadOutput, Message: "bad output placeholder"}
		}
		prod, ok := g.produced[n]
		if !ok {
			return &grok.Error{Code: grok.CodeBadOutput, Message: "grok called reference_to_video before the image output existed"}
		}
		if samePath(got, prod) {
			return nil
		}
		if g.known(got) {
			return &grok.Error{Code: grok.CodeBadOutput, Message: "grok first_frame was not the image output"}
		}
		return unsafePath(got)
	}
	if !g.known(got) {
		return unsafePath(got)
	}
	if want != "" && !samePath(want, got) {
		return &grok.Error{Code: grok.CodeBadOutput, Message: "grok image path did not match the plan"}
	}
	return nil
}

// known reports whether p is a staged input or a file this run produced.
// URLs, data URIs, [Image #N], and relative paths are not known. A wrong
// false rejects a staged file; a wrong true would accept /etc/hosts.
func (g *Guard) known(p string) bool {
	if badToken(p) {
		return false
	}
	for _, in := range g.plan.Inputs {
		if samePath(p, in) {
			return true
		}
	}
	for _, prod := range g.produced {
		if samePath(p, prod) {
			return true
		}
	}
	return false
}

// outputPlaceholder reports whether p is exactly $OUTPUT_ followed by digits.
// The plan writes that token for a file this run has not produced yet. It is
// not an outside path: onePath returns grok_bad_output until the file exists.
// Any other spelling, including a path that merely starts with that prefix,
// returns false and must be staged or already produced. An empty p returns false.
func outputPlaceholder(p string) bool {
	rest, ok := strings.CutPrefix(p, "$OUTPUT_")
	if !ok || rest == "" {
		return false
	}
	for _, r := range rest {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// unsafePath builds the 500 error for a path that is not staged or produced.
// The runner kills the process group when OnToolUse returns this error.
func unsafePath(p string) error {
	return &grok.Error{Code: grok.CodeUnsafeInput, Message: "grok tool path " + p + " is not an input or output of this request. The process was killed."}
}

// toolAllowed reports whether name is in the allowlist. An empty allowlist allows nothing.
func toolAllowed(name string, allow []string) bool {
	for _, a := range allow {
		if a == name {
			return true
		}
	}
	return false
}

// isPathKey reports argument names that carry file paths.
func isPathKey(k string) bool {
	switch k {
	case "image", "images", "first_frame", "last_frame", "keyframes":
		return true
	default:
		return false
	}
}

// badToken reports a path the model must never send: a URL, a data URI,
// an attachment placeholder, or a relative path. An empty string is a bad token.
func badToken(p string) bool {
	t := strings.TrimSpace(p)
	if t == "" || !filepath.IsAbs(t) {
		return true
	}
	low := strings.ToLower(t)
	if strings.HasPrefix(low, "http://") || strings.HasPrefix(low, "https://") || strings.HasPrefix(low, "data:") {
		return true
	}
	if strings.HasPrefix(t, "[Image") {
		return true
	}
	return false
}

// samePath reports whether a and b are the same file. Clean equality wins.
// EvalSymlinks covers macOS /var versus /private/var when both exist.
// A missing file compares as the cleaned path only.
func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if a == b {
		return true
	}
	ae, e1 := filepath.EvalSymlinks(a)
	be, e2 := filepath.EvalSymlinks(b)
	return e1 == nil && e2 == nil && ae == be
}

// asStrings reads a string or a list of strings. Nil becomes an empty list.
// Any other shape returns an error so the caller can reject the tool input.
func asStrings(v any) ([]string, error) {
	if v == nil {
		return nil, nil
	}
	switch t := v.(type) {
	case string:
		return []string{t}, nil
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			s, ok := e.(string)
			if !ok {
				return nil, os.ErrInvalid
			}
			out = append(out, s)
		}
		return out, nil
	default:
		return nil, os.ErrInvalid
	}
}

// sameJSON reports whether a and b encode to the same JSON, or are the same number.
// 8 and 8.0 match. Strings do not match numbers. A marshal error compares as unequal
// unless both values are equal float64s.
func sameJSON(a, b any) bool {
	ab, ea := json.Marshal(a)
	bb, eb := json.Marshal(b)
	if ea == nil && eb == nil && string(ab) == string(bb) {
		return true
	}
	af, aok := a.(float64)
	bf, bok := b.(float64)
	if aok && bok {
		return af == bf
	}
	return false
}
