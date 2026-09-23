package openai

import "strings"

// Usage is the OpenAI usage object. ReasoningTokens is omitted from JSON when unset.
type Usage struct {
	// PromptTokens is grok input + cache read + cache creation.
	PromptTokens int
	// CompletionTokens is grok output tokens.
	CompletionTokens int
	// TotalTokens is PromptTokens + CompletionTokens.
	TotalTokens int
	// ReasoningTokens is set when grok reported reasoning_tokens.
	ReasoningTokens int
	// HasReasoning includes completion_tokens_details when true.
	HasReasoning bool
}

// ToolResult is one function call in an assistant message.
type ToolResult struct {
	// ID matches ^call_[A-Za-z0-9]{24}$.
	ID string
	// Name is the function name.
	Name string
	// Arguments is a JSON object string.
	Arguments string
}

// Completion is a non-streaming chat.completion body.
type Completion struct {
	// ID is chatcmpl- plus the session id with hyphens removed.
	ID string `json:"id"`
	// Object is always chat.completion.
	Object string `json:"object"`
	// Created is the request arrival time in unix seconds.
	Created int64 `json:"created"`
	// Model is the grok model that actually ran.
	Model string `json:"model"`
	// Choices has one element. n>1 is rejected before a run starts.
	Choices []Choice `json:"choices"`
	// Usage is the mapped token accounting.
	Usage usageWire `json:"usage"`
}

// Choice is choices[0].
type Choice struct {
	// Index is always 0.
	Index int `json:"index"`
	// Message is the assistant message. Content is null when ToolCalls is set.
	Message MessageOut `json:"message"`
	// FinishReason is stop, length, content_filter, or tool_calls.
	FinishReason string `json:"finish_reason"`
}

// MessageOut is the assistant message. A nil Content encodes as null.
type MessageOut struct {
	// Role is assistant.
	Role string `json:"role"`
	// Content is the reply text, or null for a tool-call message.
	Content *string `json:"content"`
	// ToolCalls is omitted for a normal text reply.
	ToolCalls []ToolCallWire `json:"tool_calls,omitempty"`
}

// ToolCallWire is one OpenAI tool call.
type ToolCallWire struct {
	// ID is the call id.
	ID string `json:"id"`
	// Type is function.
	Type string `json:"type"`
	// Function holds the name and JSON arguments string.
	Function FunctionWire `json:"function"`
}

// FunctionWire is the function payload of a tool call.
type FunctionWire struct {
	// Name is the function name.
	Name string `json:"name"`
	// Arguments is a JSON object encoded as a string.
	Arguments string `json:"arguments"`
}

// usageWire is the JSON shape of usage, with reasoning details only when present.
type usageWire struct {
	PromptTokens            int            `json:"prompt_tokens"`
	CompletionTokens        int            `json:"completion_tokens"`
	TotalTokens             int            `json:"total_tokens"`
	CompletionTokensDetails *reasoningWire `json:"completion_tokens_details,omitempty"`
}

// reasoningWire is completion_tokens_details.
type reasoningWire struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

// Chunk is one SSE chat.completion.chunk.
type Chunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
	Usage   *usageWire    `json:"usage,omitempty"`
}

// ChunkChoice is one streamed choice. FinishReason is null until the final choice chunk.
type ChunkChoice struct {
	Index        int     `json:"index"`
	Delta        Delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

// Delta is the incremental assistant update.
type Delta struct {
	Role      string      `json:"role,omitempty"`
	Content   *string     `json:"content,omitempty"`
	ToolCalls []ToolDelta `json:"tool_calls,omitempty"`
}

// ToolDelta is one full tool call inside a pseudo-stream chunk.
type ToolDelta struct {
	Index    int          `json:"index"`
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionWire `json:"function"`
}

// CompletionID builds chatcmpl-<session without hyphens>.
// An empty session id still returns a prefix so the field is never blank;
// callers should pass the grok session id whenever they have one.
func CompletionID(sessionID string) string {
	return "chatcmpl-" + strings.ReplaceAll(sessionID, "-", "")
}

// MapFinishReason maps grok stop reasons onto OpenAI finish_reason values.
// end_turn becomes stop, max_tokens becomes length, refusal becomes content_filter.
// Anything else, including an empty reason, becomes stop.
func MapFinishReason(grokReason string) string {
	switch strings.ToLower(strings.ReplaceAll(grokReason, "-", "_")) {
	case "max_tokens", "length":
		return "length"
	case "refusal":
		return "content_filter"
	case "end_turn", "endturn", "":
		return "stop"
	default:
		return "stop"
	}
}

// NewCompletion builds the non-streaming JSON body.
// content nil with calls produces a tool_calls message. usage.HasReasoning adds reasoning tokens.
func NewCompletion(id string, created int64, model, finish string, content *string, calls []ToolResult, usage Usage) Completion {
	msg := MessageOut{Role: "assistant", Content: content}
	for _, c := range calls {
		msg.ToolCalls = append(msg.ToolCalls, ToolCallWire{
			ID: c.ID, Type: "function", Function: FunctionWire{Name: c.Name, Arguments: c.Arguments},
		})
	}
	return Completion{
		ID: id, Object: "chat.completion", Created: created, Model: model,
		Choices: []Choice{{Index: 0, Message: msg, FinishReason: finish}},
		Usage:   wireUsage(usage),
	}
}

// RoleChunk is the first SSE chunk: delta.role=assistant and empty content.
func RoleChunk(id string, created int64, model string) Chunk {
	empty := ""
	return Chunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []ChunkChoice{{Index: 0, Delta: Delta{Role: "assistant", Content: &empty}}},
	}
}

// ContentChunk is one text delta.
func ContentChunk(id string, created int64, model, text string) Chunk {
	return Chunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []ChunkChoice{{Index: 0, Delta: Delta{Content: &text}}},
	}
}

// ToolChunk is one full tool call in a pseudo-stream.
func ToolChunk(id string, created int64, model string, index int, call ToolResult) Chunk {
	return Chunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []ChunkChoice{{Index: 0, Delta: Delta{ToolCalls: []ToolDelta{{
			Index: index, ID: call.ID, Type: "function",
			Function: FunctionWire{Name: call.Name, Arguments: call.Arguments},
		}}}}},
	}
}

// FinishChunk closes the choice with a finish_reason and an empty delta.
func FinishChunk(id string, created int64, model, reason string) Chunk {
	return Chunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []ChunkChoice{{Index: 0, Delta: Delta{}, FinishReason: &reason}},
	}
}

// UsageChunk is the extra chunk sent when stream_options.include_usage is true.
// Choices is an empty array, not null.
func UsageChunk(id string, created int64, model string, usage Usage) Chunk {
	u := wireUsage(usage)
	return Chunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []ChunkChoice{}, Usage: &u,
	}
}

// wireUsage copies Usage into the JSON shape.
func wireUsage(u Usage) usageWire {
	w := usageWire{PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens, TotalTokens: u.TotalTokens}
	if u.HasReasoning {
		w.CompletionTokensDetails = &reasoningWire{ReasoningTokens: u.ReasoningTokens}
	}
	return w
}

// FromGrokUsage maps a grok usage triple into OpenAI fields.
// prompt is input+cacheRead+cacheCreate, completion is output, total is their sum.
func FromGrokUsage(prompt, completion, reasoning int, hasReasoning bool) Usage {
	return Usage{
		PromptTokens: prompt, CompletionTokens: completion, TotalTokens: prompt + completion,
		ReasoningTokens: reasoning, HasReasoning: hasReasoning,
	}
}
