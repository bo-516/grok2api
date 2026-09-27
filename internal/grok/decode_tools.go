package grok

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

// ToolUse is one model tool call after its input JSON is complete.
// ID matches the later tool_result. Name is the tool. Input is the raw
// arguments object. A missing Input is an empty object, not a failure by itself;
// the media guard rejects arguments that do not match the plan.
type ToolUse struct {
	// ID is the tool call id. An empty id still emits, keyed so it is not repeated.
	ID string
	// Name is the tool name, such as image_gen.
	Name string
	// Input is the JSON object of arguments. It is never nil after a successful emit;
	// a tool that sent no input becomes {}.
	Input json.RawMessage
}

// ToolResult is the user-line tool_result that belongs to one ToolUse.
// IsError means the tool failed and Text is the error sentence.
// Inline is set when the result carried a base64 image and no separate path.
type ToolResult struct {
	// ToolUseID is the matching tool call id. Empty results are still delivered
	// so the guard can reject them instead of ignoring a file.
	ToolUseID string
	// IsError is the tool_result is_error flag. A missing flag is false.
	IsError bool
	// Text is the result string, or the joined text blocks. It often holds a
	// JSON object with a path field. It is empty when the result was only an image.
	Text string
	// Inline holds base64 images from the result blocks, decoded.
	Inline []InlineMedia
}

// InlineMedia is one image embedded in a tool_result instead of saved to a path.
type InlineMedia struct {
	// MediaType is the MIME type, such as image/png. An empty type is rejected
	// by the output check, which sniffs the bytes anyway.
	MediaType string
	// Data is the decoded image. A bad base64 string is skipped, not fatal,
	// because the path in Text may still be valid.
	Data []byte
}

// toolBuild accumulates one partial-stream tool_use until content_block_stop.
type toolBuild struct {
	// id is the tool call id from content_block_start.
	id string
	// name is the tool name.
	name string
	// buf is the concatenation of input_json_delta partial_json values.
	buf strings.Builder
	// seed is a non-empty input object already present on content_block_start.
	seed json.RawMessage
}

// noteToolStart records a tool_use content block. Other block types are ignored.
// index is the stream index. A second start at the same index replaces the first,
// which only happens if grok reuses an index after stop; the previous one has
// already been emitted.
func (d *decoder) noteToolStart(ev map[string]json.RawMessage) {
	block := nestedMap(ev, "content_block")
	if jsonString(block, "type") != "tool_use" {
		return
	}
	idx, ok := jsonInt(ev, "index")
	if !ok {
		idx = 0
	}
	b := &toolBuild{id: jsonString(block, "id"), name: jsonString(block, "name")}
	if raw, exists := block["input"]; exists && len(bytesTrim(raw)) > 0 && string(bytesTrim(raw)) != "{}" && string(bytesTrim(raw)) != "null" {
		b.seed = append(json.RawMessage(nil), raw...)
	}
	if d.toolByIndex == nil {
		d.toolByIndex = map[int]*toolBuild{}
	}
	d.toolByIndex[idx] = b
}

// consumeToolDelta appends an input_json_delta to the open tool block.
// It returns true when the delta was a tool input, so text handling is skipped.
// A delta with no open block is ignored and still reported as consumed when its
// type is input_json_delta, so it is not treated as text.
func (d *decoder) consumeToolDelta(ev map[string]json.RawMessage) bool {
	delta := nestedMap(ev, "delta")
	if jsonString(delta, "type") != "input_json_delta" {
		return false
	}
	idx, ok := jsonInt(ev, "index")
	if !ok {
		idx = 0
	}
	if d.toolByIndex == nil {
		return true
	}
	b := d.toolByIndex[idx]
	if b == nil {
		return true
	}
	b.buf.WriteString(jsonString(delta, "partial_json"))
	return true
}

// finishTool emits the tool_use for this index when the block closes.
// A stop for a text or thinking block is a no-op. The callback error is returned
// so Decode stops and the runner can kill the process.
func (d *decoder) finishTool(ev map[string]json.RawMessage) error {
	idx, ok := jsonInt(ev, "index")
	if !ok {
		idx = 0
	}
	if d.toolByIndex == nil {
		return nil
	}
	b := d.toolByIndex[idx]
	if b == nil {
		return nil
	}
	delete(d.toolByIndex, idx)
	raw := bytesTrim([]byte(b.buf.String()))
	if len(raw) == 0 {
		raw = b.seed
	}
	return d.emitTool(b.id, b.name, raw)
}

// emitAssistantTools emits tool_use blocks on an assistant line that the
// partial stream did not already deliver. The same id is skipped.
// A callback error stops Decode.
func (d *decoder) emitAssistantTools(m map[string]json.RawMessage) error {
	msg := nestedMap(m, "message")
	if msg == nil {
		return nil
	}
	var blocks []map[string]json.RawMessage
	if raw, ok := msg["content"]; ok {
		_ = json.Unmarshal(raw, &blocks)
	}
	for _, block := range blocks {
		if jsonString(block, "type") != "tool_use" {
			continue
		}
		var raw json.RawMessage
		if in, ok := block["input"]; ok {
			raw = in
		}
		if err := d.emitTool(jsonString(block, "id"), jsonString(block, "name"), raw); err != nil {
			return err
		}
	}
	return nil
}

// emitTool calls OnToolUse once per id. input that is empty or not a JSON object
// becomes {}. A repeated id returns nil. A nil OnToolUse still records the id
// so a later assistant copy is not emitted either; chat runs leave the callback nil.
func (d *decoder) emitTool(id, name string, input json.RawMessage) error {
	key := id
	if key == "" {
		key = name + ":" + string(input)
	}
	if d.toolSeen == nil {
		d.toolSeen = map[string]bool{}
	}
	if d.toolSeen[key] {
		return nil
	}
	d.toolSeen[key] = true
	if d.ev.OnToolUse == nil {
		return nil
	}
	in := bytesTrim(input)
	if len(in) == 0 || !json.Valid(in) {
		in = []byte("{}")
	}
	return d.ev.OnToolUse(ToolUse{ID: id, Name: name, Input: append(json.RawMessage(nil), in...)})
}

// onToolUser delivers every tool_result in a user message.
// Other user content is ignored so a chat transcript that contains a user line
// still decodes. A callback error stops Decode.
func (d *decoder) onToolUser(m map[string]json.RawMessage) error {
	d.noteSession(jsonString(m, "session_id", "sessionId"))
	msg := nestedMap(m, "message")
	if msg == nil {
		return nil
	}
	var blocks []map[string]json.RawMessage
	if raw, ok := msg["content"]; ok {
		if json.Unmarshal(raw, &blocks) != nil {
			return nil
		}
	}
	for _, block := range blocks {
		if jsonString(block, "type") != "tool_result" {
			continue
		}
		tr := ToolResult{
			ToolUseID: jsonString(block, "tool_use_id", "toolUseId"),
			IsError:   jsonBool(block, "is_error"),
		}
		fillResult(&tr, block["content"])
		if d.ev.OnToolResult != nil {
			if err := d.ev.OnToolResult(tr); err != nil {
				return err
			}
		}
	}
	return nil
}

// fillResult copies text and inline images from a tool_result content field.
// A JSON string is Text. An array of blocks contributes text and base64 images.
// A missing content leaves Text empty.
func fillResult(tr *ToolResult, raw json.RawMessage) {
	if len(bytesTrim(raw)) == 0 {
		return
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		tr.Text = s
		return
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return
	}
	var b strings.Builder
	for _, block := range blocks {
		switch jsonString(block, "type") {
		case "text":
			b.WriteString(jsonString(block, "text"))
		case "image":
			if media, ok := inlineImage(block); ok {
				tr.Inline = append(tr.Inline, media)
			}
		}
	}
	tr.Text = b.String()
}

// inlineImage decodes a base64 image block. source.type base64 is the shape
// grok uses for an inline image. A missing or bad payload returns false.
func inlineImage(block map[string]json.RawMessage) (InlineMedia, bool) {
	src := nestedMap(block, "source")
	if src == nil {
		return InlineMedia{}, false
	}
	payload := jsonString(src, "data")
	if payload == "" {
		return InlineMedia{}, false
	}
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		data, err = base64.RawStdEncoding.DecodeString(payload)
		if err != nil {
			return InlineMedia{}, false
		}
	}
	return InlineMedia{MediaType: jsonString(src, "media_type", "mediaType"), Data: data}, true
}

// bytesTrim trims ASCII space from a raw JSON value. Nil stays nil.
func bytesTrim(raw []byte) []byte {
	return []byte(strings.TrimSpace(string(raw)))
}
