package openai

// ToolCallChunks is the SSE sequence for one or more tool calls.
// The first chunk carries role and content null when content is empty, plus the first
// call's id, type, and name with arguments "". Each call then gets an arguments chunk.
// A non-empty content is its own role chunk before the tool fragments.
// The last chunk has finish_reason tool_calls. calls must be non-empty.
func ToolCallChunks(id string, created int64, model, content string, calls []ToolResult) []Chunk {
	var chunks []Chunk
	if content != "" {
		chunks = append(chunks, textStart(id, created, model, content))
	}
	for i, call := range calls {
		start := Delta{ToolCalls: []ToolDelta{{
			Index: i, ID: call.ID, Type: "function",
			Function: functionDelta{Name: strPtr(call.Name), Arguments: strPtr("")},
		}}}
		if i == 0 && content == "" {
			start.Role = "assistant"
			start.NullContent = true
		}
		chunks = append(chunks, oneChunk(id, created, model, start))
		chunks = append(chunks, oneChunk(id, created, model, Delta{ToolCalls: []ToolDelta{{
			Index: i, Function: functionDelta{Arguments: strPtr(call.Arguments)},
		}}}))
	}
	chunks = append(chunks, FinishChunk(id, created, model, "tool_calls"))
	return chunks
}

// FunctionCallChunks is the legacy SSE sequence for a single function_call.
// The shape matches the deprecated functions API: content null, then name, then arguments.
// finish_reason is function_call. content is a preamble chunk when it is non-empty.
func FunctionCallChunks(id string, created int64, model, content string, call ToolResult) []Chunk {
	var chunks []Chunk
	start := Delta{FunctionCall: &FunctionCallDelta{Name: strPtr(call.Name), Arguments: strPtr("")}}
	if content != "" {
		chunks = append(chunks, textStart(id, created, model, content))
	} else {
		start.Role = "assistant"
		start.NullContent = true
	}
	chunks = append(chunks, oneChunk(id, created, model, start))
	chunks = append(chunks, oneChunk(id, created, model, Delta{FunctionCall: &FunctionCallDelta{Arguments: strPtr(call.Arguments)}}))
	chunks = append(chunks, FinishChunk(id, created, model, "function_call"))
	return chunks
}

// textStart is the first chunk of a reply that has visible text: role plus that text.
func textStart(id string, created int64, model, content string) Chunk {
	return oneChunk(id, created, model, Delta{Role: "assistant", Content: strPtr(content)})
}

// oneChunk wraps a delta as a single choice with a null finish_reason.
func oneChunk(id string, created int64, model string, delta Delta) Chunk {
	return Chunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []ChunkChoice{{Index: 0, Delta: delta}},
	}
}

// strPtr returns a pointer to s so omitempty keeps an empty arguments string.
func strPtr(s string) *string { return &s }
