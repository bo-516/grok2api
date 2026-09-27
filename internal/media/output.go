package media

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/shaoboli/agent-mock/internal/grok"
)

// pathRe finds an absolute media path inside a tool_result string.
// It is the fallback when the result is not a JSON object with a path field.
var pathRe = regexp.MustCompile(`(?i)(/[^\s"'<>]+\.(?:jpg|jpeg|png|webp|mp4))`)

// taken is a validated output ready to copy. Path is set for a session file.
// Inline is set when the result embedded an image and named no path.
type taken struct {
	// Path is the regular file inside the session directory. Empty for inline.
	Path string
	// Inline is decoded image bytes. Empty when Path is set.
	Inline []byte
	// Limit is the size cap, 50 MiB for an image and 500 MiB for a video.
	Limit int64
}

// takeOutput validates tr and returns the file or inline bytes to store.
// sessionsRoot is <GROK_HOME>/sessions. sessionID must appear as a path element.
// An error result is classified with ClassifyMedia. A bad path or sniff is
// grok_bad_output. Nothing is stored by this function.
func takeOutput(tr grok.ToolResult, sessionsRoot, sessionID string) (taken, error) {
	if tr.IsError {
		return taken{}, grok.ClassifyMedia(tr.Text)
	}
	if p := resultPath(tr.Text); p != "" {
		limit, err := vetPath(p, sessionsRoot, sessionID)
		if err != nil {
			return taken{}, err
		}
		return taken{Path: filepath.Clean(p), Limit: limit}, nil
	}
	for _, in := range tr.Inline {
		if len(in.Data) == 0 {
			continue
		}
		if _, _, ok := sniff(in.Data); !ok {
			continue
		}
		return taken{Inline: in.Data, Limit: maxImage}, nil
	}
	return taken{}, &grok.Error{Code: grok.CodeBadOutput, Message: "grok tool result did not include a media file"}
}

// resultPath returns the output path from a tool_result string.
// A JSON object with a string "path" field wins. Otherwise the first absolute
// media path in the text is used. An empty text returns "".
func resultPath(text string) string {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "{") {
		var m map[string]any
		if json.Unmarshal([]byte(text), &m) == nil {
			if p, ok := m["path"].(string); ok && p != "" {
				return p
			}
		}
	}
	if m := pathRe.FindStringSubmatch(text); m != nil {
		return m[1]
	}
	return ""
}

// vetPath checks that path is a regular file under sessionsRoot and contains
// sessionID. The size cap comes from the sniffed type: 50 MiB for an image and
// 500 MiB for a video. The filename suffix is not the cap, so a PNG renamed
// to .mp4 is still an image. A symlink, a path outside the session, a bad
// sniff, or a file over its cap is grok_bad_output. The returned limit is the
// cap the caller must pass to the store. The file is not copied here.
func vetPath(path, sessionsRoot, sessionID string) (int64, error) {
	clean := filepath.Clean(path)
	if sessionID == "" || sessionsRoot == "" || !filepath.IsAbs(clean) {
		return 0, &grok.Error{Code: grok.CodeBadOutput, Message: "grok tool result path is not in this session"}
	}
	root := filepath.Clean(sessionsRoot)
	rel, err := filepath.Rel(root, clean)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return 0, &grok.Error{Code: grok.CodeBadOutput, Message: "grok tool result path is outside the session directory"}
	}
	found := false
	for _, part := range strings.Split(rel, string(os.PathSeparator)) {
		if part == sessionID {
			found = true
			break
		}
	}
	if !found {
		return 0, &grok.Error{Code: grok.CodeBadOutput, Message: "grok tool result path is not in this session"}
	}
	st, err := os.Lstat(clean)
	if err != nil {
		return 0, &grok.Error{Code: grok.CodeBadOutput, Message: "grok tool result path could not be read", Err: err}
	}
	if st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
		return 0, &grok.Error{Code: grok.CodeBadOutput, Message: "grok tool result path is not a regular file"}
	}
	f, err := os.Open(clean)
	if err != nil {
		return 0, &grok.Error{Code: grok.CodeBadOutput, Message: "grok tool result path could not be read", Err: err}
	}
	defer f.Close()
	buf := make([]byte, 512)
	n, _ := f.Read(buf)
	ext, _, ok := sniff(buf[:n])
	if !ok {
		return 0, &grok.Error{Code: grok.CodeBadOutput, Message: "grok tool result is not png, jpeg, webp, or mp4"}
	}
	limit := int64(maxImage)
	if ext == "mp4" {
		limit = maxVideo
	}
	if st.Size() > limit {
		return 0, &grok.Error{Code: grok.CodeBadOutput, Message: "grok tool result file exceeds the size limit"}
	}
	return limit, nil
}
