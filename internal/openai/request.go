// Package openai is the Chat Completions subset agent-mock accepts and returns.
// It is not the OpenAI SDK. The SDK is only used by tests and the example.
package openai

import "encoding/json"

// maxInline is the largest system text or JSON schema placed in grok's argv.
// Bigger system text moves into the prompt file. A bigger schema is rejected.
const MaxArgBytes = 96 * 1024

// RequestError is a client mistake. Status, Type, and Code match the OpenAI error table.
// Message names the field when the call used an unsupported parameter or content part.
type RequestError struct {
	// Status is the HTTP status.
	Status int
	// Type is error.type, usually invalid_request_error.
	Type string
	// Code is error.code.
	Code string
	// Message is error.message.
	Message string
}

// Error returns Message so handlers can log the same text they send.
func (e *RequestError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

// Request is a validated chat completion call.
// Ignored lists sampling fields that were accepted and will not be sent to grok.
type Request struct {
	// Model is the caller model id before alias resolution.
	Model string
	// Messages are the conversation in request order. System and developer stay in this list;
	// rendering splits them out. Text is already joined from text parts.
	Messages []Message
	// Stream selects SSE. Headers are withheld until grok message_start when events are real.
	Stream bool
	// IncludeUsage adds an empty-choices usage chunk. It is only meaningful when Stream is true.
	IncludeUsage bool
	// Tools are function tools. Empty means a normal text completion.
	Tools []ToolDef
	// ToolChoice is auto when tools are present and the caller omitted the field.
	// Kind none skips the envelope even if Tools is non-empty.
	ToolChoice ToolChoice
	// Parallel is nil when the caller omitted parallel_tool_calls (OpenAI default: allow many).
	// A non-nil false forces the envelope to maxItems 1.
	Parallel *bool
	// ResponseFormat is nil for plain text. json_object and json_schema set Schema.
	ResponseFormat *ResponseFormat
	// ReasoningEffort is a legal effort or empty. Illegal values never reach this struct.
	ReasoningEffort string
	// Ignored is the sorted-by-table list of inert sampling fields present on the request.
	Ignored []string
}

// Message is one chat message after content parts are reduced to text.
type Message struct {
	// Role is system, developer, user, assistant, or tool.
	Role string
	// Text is the string content, or text parts joined with newlines.
	Text string
	// ToolCalls is set on assistant messages that invoked functions.
	ToolCalls []ToolCall
	// ToolCallID links a tool result to the assistant call id.
	ToolCallID string
}

// ToolCall is an assistant function call already in the conversation.
type ToolCall struct {
	// ID is the call id the caller supplied, such as call_abc.
	ID string
	// Name is the function name.
	Name string
	// Arguments is the JSON arguments string.
	Arguments string
}

// ToolDef is one function the model may call.
type ToolDef struct {
	// Name is the function name the envelope will allow.
	Name string
	// Description is shown to the model in the system text.
	Description string
	// Parameters is the JSON schema for arguments. Empty means an object.
	Parameters json.RawMessage
}

// ToolChoice selects how tools are used.
// Kind is auto, none, required, or function. Name is set only for function.
type ToolChoice struct {
	// Kind is auto, none, required, or function.
	Kind string
	// Name is the only allowed function when Kind is function.
	Name string
}

// ResponseFormat is json_object or json_schema.
type ResponseFormat struct {
	// Type is json_object or json_schema.
	Type string
	// Name is the schema name from json_schema.name. It is not sent to grok.
	Name string
	// Schema is the JSON schema passed to --json-schema, or embedded in the tool envelope.
	Schema json.RawMessage
}

// bad builds a 400 invalid_request_error.
func bad(code, message string) *RequestError {
	return &RequestError{Status: 400, Type: "invalid_request_error", Code: code, Message: message}
}
