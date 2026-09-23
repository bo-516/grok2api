package grok

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// Usage is grok's token accounting. Prompt tokens for the OpenAI response are
// Input + CacheRead + CacheCreate. Completion tokens are Output.
// ReasoningSet is true when grok sent reasoning_tokens, even if the count is 0.
type Usage struct {
	// Input is usage.input_tokens.
	Input int
	// CacheRead is usage.cache_read_input_tokens.
	CacheRead int
	// CacheCreate is usage.cache_creation_input_tokens.
	CacheCreate int
	// Output is usage.output_tokens.
	Output int
	// Reasoning is usage.reasoning_tokens when ReasoningSet is true.
	Reasoning int
	// ReasoningSet reports whether the reasoning_tokens field was present.
	ReasoningSet bool
}

// PromptTokens is the OpenAI prompt_tokens value.
func (u Usage) PromptTokens() int { return u.Input + u.CacheRead + u.CacheCreate }

// CompletionTokens is the OpenAI completion_tokens value.
func (u Usage) CompletionTokens() int { return u.Output }

// TotalTokens is prompt plus completion, not grok's own total field.
func (u Usage) TotalTokens() int { return u.PromptTokens() + u.CompletionTokens() }

// Final is a finished grok run. Text is the assistant reply with thinking removed.
// Structured is the structured_output object when grok sent one, otherwise nil.
// StopReason is grok's raw reason (end_turn, max_tokens, refusal); the HTTP layer maps it.
type Final struct {
	// Text is the assistant text. Thinking deltas are not included.
	Text string
	// Structured is raw structured_output JSON, or nil.
	Structured json.RawMessage
	// StopReason is grok's stop reason, not the OpenAI finish_reason.
	StopReason string
	// SessionID is the grok session id, with hyphens. Empty if grok never assigned one.
	SessionID string
	// Model is the grok model that produced the run. Empty if grok did not say.
	Model string
	// Usage is the token accounting from the result record.
	Usage Usage
}

// Init is the first streaming record. ToolsRaw must be exactly [] before the run continues.
// A missing or null tools field is not proof of an empty tool list.
type Init struct {
	// ToolsRaw is the JSON array from system/init, such as [] or ["run_terminal_command"].
	ToolsRaw json.RawMessage
	// Model is the model grok says it loaded.
	Model string
	// SessionID is the session id from the init line.
	SessionID string
	// PermissionMode is the mode grok echoed. The argv is still dontAsk even when
	// a not-logged-in init echoes "default".
	PermissionMode string
}

// Events receives incremental records. Nil funcs are skipped.
// A non-nil error from a func stops Decode and is returned to the runner,
// which kills the process group. OnInit is how the runner rejects a non-empty tool list.
type Events struct {
	// OnInit is called for the first system/init object.
	OnInit func(Init) error
	// OnMessageStart is called when the model starts a message, before text deltas.
	// The HTTP layer writes SSE headers here so earlier failures stay normal status codes.
	OnMessageStart func(sessionID, model string) error
	// OnText is called with each text delta. Thinking deltas are not forwarded.
	OnText func(delta string) error
}

// errStop is returned by the runner's OnInit when StopAfterInit is set.
// Decode treats it as a clean stop. It is not a client-facing error.
var errStop = errors.New("stop after init")

// Decode reads NDJSON or one JSON value from r until a terminal record or EOF.
// Pretty-printed text.json and line-delimited streaming-messages-json both work
// because encoding/json reads one value at a time.
// A system/init record invokes OnInit. text_delta invokes OnText. Thinking is dropped.
// The returned Final.Text prefers streamed text, then the result string, then the text field.
// An error record or is_error result returns a classified *Error and a Final that
// still carries SessionID so the caller can delete the session.
func Decode(r io.Reader, ev Events) (Final, error) {
	dec := json.NewDecoder(r)
	dec.UseNumber()
	st := &decoder{ev: ev}
	for {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			if st.finalReady || st.failed != nil {
				break
			}
			// A cut-off or non-JSON value is not a completion, even if text deltas
			// were already delivered. Empty stdout hits the sawAny check below instead.
			out := st.finish()
			return out, &Error{Code: CodeBadOutput, Message: "grok output could not be parsed", SessionID: out.SessionID, Err: err}
		}
		if err := st.consume(raw); err != nil {
			if errors.Is(err, errStop) {
				return st.finish(), errStop
			}
			if ge, ok := AsError(err); ok {
				if ge.SessionID == "" {
					ge.SessionID = st.sessionID
				}
				return st.finish(), ge
			}
			return st.finish(), err
		}
		if st.failed != nil || st.finalReady {
			// Keep reading in case a later result is richer? Terminal records end the run.
			// One extra value is possible (assistant then result). finalReady is set on result
			// and on a bare text.json object. Do not stop before result if we only saw assistant.
			if st.terminal {
				break
			}
		}
	}
	out := st.finish()
	if st.failed != nil {
		if st.failed.SessionID == "" {
			st.failed.SessionID = out.SessionID
		}
		return out, st.failed
	}
	if !st.sawAny {
		return out, Failed("", io.ErrUnexpectedEOF)
	}
	if !st.terminal && !st.finalReady {
		return out, &Error{Code: CodeBadOutput, Message: "grok output ended without a result", SessionID: out.SessionID}
	}
	return out, nil
}

// decoder accumulates one grok stdout stream.
type decoder struct {
	// ev is the caller callbacks.
	ev Events
	// text is the concatenation of text deltas.
	text strings.Builder
	// fallback is assistant text or the text field when no deltas arrived.
	fallback string
	// resultText is the result.result string.
	resultText string
	// structured is structured_output.
	structured json.RawMessage
	// stopReason is the latest stop reason.
	stopReason string
	// sessionID is the latest non-empty session id.
	sessionID string
	// model is the latest usable model id.
	model string
	// usage is the latest usage object.
	usage Usage
	// sawAny is true after the first JSON value.
	sawAny bool
	// sawInit is true after a system/init record. Streaming records before that
	// are rejected: the first NDJSON record must prove tools is [].
	sawInit bool
	// finalReady is true once a success payload exists.
	finalReady bool
	// terminal is true after a result or a single-object success/error.
	terminal bool
	// failed is a classified error from an error record.
	failed *Error
}

// consume handles one JSON value. It returns errStop, a *Error, or a callback error.
func (d *decoder) consume(raw json.RawMessage) error {
	d.sawAny = true
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return &Error{Code: CodeBadOutput, Message: "grok emitted invalid JSON"}
	}
	typ := jsonString(m, "type")
	switch typ {
	case "system":
		if jsonString(m, "subtype") != "init" && !d.sawInit {
			return d.missingInit()
		}
		return d.onSystem(m)
	case "stream_event":
		if err := d.missingInit(); err != nil {
			return err
		}
		return d.onEvent(m)
	case "assistant":
		if err := d.missingInit(); err != nil {
			return err
		}
		d.onAssistant(m)
		return nil
	case "result":
		if err := d.missingInit(); err != nil {
			return err
		}
		return d.onResult(m)
	case "error":
		d.noteSession(jsonString(m, "session_id", "sessionId"))
		d.failed = Classify(jsonString(m, "message"), "")
		d.failed.SessionID = d.sessionID
		d.terminal = true
		return nil
	default:
		if _, ok := m["text"]; ok && typ == "" {
			d.onBare(m)
			d.terminal = true
			d.finalReady = true
			return nil
		}
		return nil
	}
}

// missingInit rejects a streaming record that arrived before system/init.
// A bare output-format json object has no type field and is not checked here.
// The runner kills the process group when it sees this error.
func (d *decoder) missingInit() error {
	if d.sawInit {
		return nil
	}
	return &Error{
		Code:      CodeUnsafe,
		Message:   "grok streaming output had no system/init record with tools []. The process was killed.",
		SessionID: d.sessionID,
	}
}

// onSystem handles system/init. Other subtypes are ignored once init was seen.
// A non-init system record does not prove the tool list.
func (d *decoder) onSystem(m map[string]json.RawMessage) error {
	if jsonString(m, "subtype") != "init" {
		return nil
	}
	d.sawInit = true
	d.noteSession(jsonString(m, "session_id", "sessionId"))
	d.noteModel(jsonString(m, "model"))
	info := Init{
		ToolsRaw:       m["tools"],
		Model:          d.model,
		SessionID:      d.sessionID,
		PermissionMode: jsonString(m, "permissionMode", "permission_mode"),
	}
	if d.ev.OnInit != nil {
		return d.ev.OnInit(info)
	}
	return nil
}

// onEvent handles Anthropic-style stream_event records.
// message_start opens the HTTP stream. text_delta is forwarded. thinking_delta is dropped.
func (d *decoder) onEvent(m map[string]json.RawMessage) error {
	d.noteSession(jsonString(m, "session_id", "sessionId"))
	var ev map[string]json.RawMessage
	if raw, ok := m["event"]; ok {
		_ = json.Unmarshal(raw, &ev)
	}
	switch jsonString(ev, "type") {
	case "message_start":
		if msg := nestedMap(ev, "message"); msg != nil {
			d.noteModel(jsonString(msg, "model"))
		}
		if d.ev.OnMessageStart != nil {
			return d.ev.OnMessageStart(d.sessionID, d.model)
		}
	case "content_block_delta":
		delta := nestedMap(ev, "delta")
		if jsonString(delta, "type") == "text_delta" {
			piece := jsonString(delta, "text")
			if piece != "" {
				d.text.WriteString(piece)
				if d.ev.OnText != nil {
					return d.ev.OnText(piece)
				}
			}
		}
	case "message_delta":
		if delta := nestedMap(ev, "delta"); delta != nil {
			if reason := jsonString(delta, "stop_reason", "stopReason"); reason != "" {
				d.stopReason = reason
			}
		}
		d.usage = mergeUsage(d.usage, ev["usage"])
	}
	return nil
}

// onAssistant stores text blocks as a fallback when deltas were not streamed.
func (d *decoder) onAssistant(m map[string]json.RawMessage) {
	d.noteSession(jsonString(m, "session_id", "sessionId"))
	msg := nestedMap(m, "message")
	if msg == nil {
		return
	}
	d.noteModel(jsonString(msg, "model"))
	if reason := jsonString(msg, "stop_reason", "stopReason"); reason != "" {
		d.stopReason = reason
	}
	d.usage = mergeUsage(d.usage, msg["usage"])
	var blocks []map[string]json.RawMessage
	if raw, ok := msg["content"]; ok {
		_ = json.Unmarshal(raw, &blocks)
	}
	var b strings.Builder
	for _, block := range blocks {
		if jsonString(block, "type") == "text" {
			b.WriteString(jsonString(block, "text"))
		}
	}
	if b.Len() > 0 {
		d.fallback = b.String()
	}
}

// onResult records the terminal success or error object.
func (d *decoder) onResult(m map[string]json.RawMessage) error {
	d.noteSession(jsonString(m, "session_id", "sessionId"))
	d.resultText = jsonString(m, "result")
	if reason := jsonString(m, "stop_reason", "stopReason"); reason != "" {
		d.stopReason = reason
	}
	if raw, ok := m["structured_output"]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		d.structured = append(json.RawMessage(nil), raw...)
	}
	d.usage = mergeUsage(d.usage, m["usage"])
	d.noteModelUsage(m["modelUsage"])
	d.terminal = true
	if jsonBool(m, "is_error") || strings.Contains(jsonString(m, "subtype"), "error") {
		d.failed = Classify(firstError(m), "")
		d.failed.SessionID = d.sessionID
		return nil
	}
	d.finalReady = true
	return nil
}

// onBare handles output-format json (text.json): one object with text, stopReason, sessionId, usage.
func (d *decoder) onBare(m map[string]json.RawMessage) {
	d.noteSession(jsonString(m, "sessionId", "session_id"))
	d.fallback = jsonString(m, "text")
	if reason := jsonString(m, "stopReason", "stop_reason"); reason != "" {
		d.stopReason = reason
	}
	if raw, ok := m["structuredOutput"]; ok && string(bytes.TrimSpace(raw)) != "null" {
		d.structured = append(json.RawMessage(nil), raw...)
	}
	if raw, ok := m["structured_output"]; ok && string(bytes.TrimSpace(raw)) != "null" {
		d.structured = append(json.RawMessage(nil), raw...)
	}
	d.usage = mergeUsage(d.usage, m["usage"])
	d.noteModelUsage(m["modelUsage"])
	if jsonString(m, "type") == "error" || jsonBool(m, "is_error") {
		d.failed = Classify(jsonString(m, "message"), "")
	}
}

// finish builds the Final from accumulated fields.
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

// noteSession keeps the first non-empty session id, then replaces an empty one only.
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
