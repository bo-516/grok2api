package grok

import (
	"bytes"
	"encoding/json"
	"strings"
)

// firstError joins result.errors or falls back to the message or result text.
// An errors array is how streaming runs report "unknown model" and "not signed in".
func firstError(m map[string]json.RawMessage) string {
	if raw, ok := m["errors"]; ok {
		var list []string
		if json.Unmarshal(raw, &list) == nil && len(list) > 0 {
			return strings.Join(list, "\n")
		}
	}
	if msg := jsonString(m, "message"); msg != "" {
		return msg
	}
	return jsonString(m, "result")
}

// jsonString returns the first key that holds a JSON string.
// A missing key or a non-string value returns "".
func jsonString(m map[string]json.RawMessage, keys ...string) string {
	if m == nil {
		return ""
	}
	for _, k := range keys {
		raw, ok := m[k]
		if !ok {
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
	}
	return ""
}

// jsonBool returns a JSON bool field. Missing or non-bool is false,
// so a missing is_error does not look like a failure.
func jsonBool(m map[string]json.RawMessage, key string) bool {
	raw, ok := m[key]
	if !ok {
		return false
	}
	var b bool
	_ = json.Unmarshal(raw, &b)
	return b
}

// nestedMap unmarshals a nested object. A missing or non-object value returns nil.
func nestedMap(m map[string]json.RawMessage, key string) map[string]json.RawMessage {
	raw, ok := m[key]
	if !ok {
		return nil
	}
	var out map[string]json.RawMessage
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	return out
}

// mergeUsage overlays a usage object onto prev. A missing reasoning_tokens key
// does not clear ReasoningSet if an earlier object already set it.
func mergeUsage(prev Usage, raw json.RawMessage) Usage {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return prev
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return prev
	}
	next := prev
	if v, ok := jsonInt(m, "input_tokens"); ok {
		next.Input = v
	}
	if v, ok := jsonInt(m, "cache_read_input_tokens"); ok {
		next.CacheRead = v
	}
	if v, ok := jsonInt(m, "cache_creation_input_tokens"); ok {
		next.CacheCreate = v
	}
	if v, ok := jsonInt(m, "output_tokens"); ok {
		next.Output = v
	}
	if v, ok := jsonInt(m, "reasoning_tokens"); ok {
		next.Reasoning = v
		next.ReasoningSet = true
	}
	return next
}

// finish builds the Final from accumulated fields.
// Streamed text wins, then the result string, then assistant text.
func (d *decoder) finish() Final {
	text := d.text.String()
	if text == "" {
		text = d.resultText
	}
	if text == "" {
		text = d.fallback
	}
	return Final{
		Text:       text,
		Structured: d.structured,
		StopReason: d.stopReason,
		SessionID:  d.sessionID,
		Model:      d.model,
		Usage:      d.usage,
	}
}

// noteSession keeps the latest non-empty session id.
// An empty id does not clear one already seen.
func (d *decoder) noteSession(id string) {
	if id != "" {
		d.sessionID = id
	}
}

// noteModel ignores blank and "unknown", which a logged-out init uses as a placeholder.
func (d *decoder) noteModel(model string) {
	if model != "" && model != "unknown" {
		d.model = model
	}
}

// noteModelUsage reads the single key of modelUsage as the model id.
// Zero or many keys leave the model unchanged. A bad object is ignored.
func (d *decoder) noteModelUsage(raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var models map[string]json.RawMessage
	if err := json.Unmarshal(raw, &models); err != nil {
		return
	}
	if len(models) == 1 {
		for name := range models {
			d.noteModel(name)
		}
	}
}

// jsonInt reads an integer. JSON numbers may arrive as json.Number. Non-numbers return false.
func jsonInt(m map[string]json.RawMessage, key string) (int, bool) {
	raw, ok := m[key]
	if !ok {
		return 0, false
	}
	var n int
	if json.Unmarshal(raw, &n) == nil {
		return n, true
	}
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		return int(f), true
	}
	return 0, false
}
