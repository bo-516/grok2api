// Package media plans grok media runs, checks each tool call, and stores the files.
package media

import (
	"bytes"
	"net/http"
)

// sniff reads the leading bytes of an image or video and reports the store
// extension and Content-Type. ok is false for anything that is not png, jpeg,
// webp, or mp4. prefix may be shorter than 512 bytes; detection uses what it is given.
// A wrong ok lets a text file into the store, so callers must treat false as a reject.
func sniff(prefix []byte) (ext, ctype string, ok bool) {
	if len(prefix) >= 12 && bytes.Equal(prefix[4:8], []byte("ftyp")) {
		return "mp4", "video/mp4", true
	}
	ctype = http.DetectContentType(prefix)
	switch ctype {
	case "image/png":
		return "png", ctype, true
	case "image/jpeg":
		return "jpg", ctype, true
	case "image/webp":
		return "webp", ctype, true
	case "video/mp4":
		return "mp4", ctype, true
	default:
		return "", "", false
	}
}
