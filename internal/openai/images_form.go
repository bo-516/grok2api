package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
)

// maxPart is one uploaded image. The whole request is limited separately to 64 MiB.
const maxPart = 20 << 20

// ParseImageEditMultipart reads an OpenAI Images.Edit body.
// The file field is image for one file and image[] for several, at most 5.
// mask is 400 and says image_edit has no mask. A part over 20 MiB is invalid_image.
// contentType is the request Content-Type, including the boundary.
func ParseImageEditMultipart(body []byte, contentType string) (ImageEdit, error) {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil || params["boundary"] == "" {
		return ImageEdit{}, bad("invalid_json", "multipart body has no boundary")
	}
	r := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	fields := map[string]json.RawMessage{}
	var sources []EditSource
	for {
		part, err := r.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return ImageEdit{}, bad("invalid_json", "could not read the multipart body")
		}
		name := part.FormName()
		if name == "mask" {
			return ImageEdit{}, bad("unsupported_parameter", `parameter "mask" is unsupported: image_edit has no mask`)
		}
		slurp, err := io.ReadAll(io.LimitReader(part, maxPart+1))
		part.Close()
		if err != nil {
			return ImageEdit{}, bad("invalid_image", "could not read an image part")
		}
		if isFileField(name) {
			if int64(len(slurp)) > maxPart {
				return ImageEdit{}, bad("invalid_image", "image exceeds 20 MiB")
			}
			if len(slurp) == 0 {
				return ImageEdit{}, bad("invalid_image", "image is empty")
			}
			sources = append(sources, EditSource{Body: slurp, Name: part.FileName()})
			continue
		}
		fields[name] = json.RawMessage(strconvQuote(string(slurp)))
	}
	if len(sources) == 0 {
		return ImageEdit{}, bad("invalid_image", "image is required")
	}
	if len(sources) > 5 {
		return ImageEdit{}, bad("invalid_image", "at most 5 images are accepted")
	}
	if _, ok := fields["file_id"]; ok {
		return ImageEdit{}, bad("unsupported_parameter", `parameter "file_id" is unsupported`)
	}
	prompt, err := needPrompt(fields, true)
	if err != nil {
		return ImageEdit{}, err
	}
	n, err := needN(fields)
	if err != nil {
		return ImageEdit{}, err
	}
	if err := rejectMediaFlags(fields); err != nil {
		return ImageEdit{}, err
	}
	return finishEdit(fields, prompt, n, sources)
}

// IsMultipart reports whether Content-Type is multipart/form-data.
// A JSON edit uses the other parser. An empty type is not multipart.
func IsMultipart(contentType string) bool {
	media, _, err := mime.ParseMediaType(contentType)
	return err == nil && media == "multipart/form-data"
}

// isFileField is the OpenAI file part name: image, image[], or image[0].
func isFileField(name string) bool {
	return name == "image" || name == "image[]" || strings.HasPrefix(name, "image[")
}

// strconvQuote JSON-encodes a form field so the JSON helpers can read it.
// A value that cannot be a JSON string is returned as an empty JSON string.
func strconvQuote(s string) json.RawMessage {
	b, err := json.Marshal(s)
	if err != nil {
		return json.RawMessage(`""`)
	}
	return b
}

// TooLarge reports whether err is an http.MaxBytesError.
// Handlers turn that into 413 request_too_large. Other read errors are not too-large.
func TooLarge(err error) bool {
	var maxErr *http.MaxBytesError
	return errors.As(err, &maxErr)
}
