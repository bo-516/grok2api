// Command go-openai calls a local agent-mock with openai-go.
// Set OPENAI_BASE_URL=http://127.0.0.1:8787/v1 and OPENAI_API_KEY=dev.
// It prints a text reply, a streamed reply, one tool call, and one JSON object.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

// main runs the four local-dev calls and exits non-zero if any reply is empty.
func main() {
	base := os.Getenv("OPENAI_BASE_URL")
	key := os.Getenv("OPENAI_API_KEY")
	if base == "" || key == "" {
		fmt.Fprintln(os.Stderr, "set OPENAI_BASE_URL and OPENAI_API_KEY")
		os.Exit(1)
	}
	client := openai.NewClient(option.WithBaseURL(base), option.WithAPIKey(key), option.WithMaxRetries(0))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	text, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model:    "gpt-4o-mini",
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("Reply with exactly the word: pong")},
	})
	if err != nil || text.Choices[0].Message.Content == "" {
		if err == nil {
			err = fmt.Errorf("empty content")
		}
		fail("text", err)
	}
	fmt.Println(text.Choices[0].Message.Content)

	stream := client.Chat.Completions.NewStreaming(ctx, openai.ChatCompletionNewParams{
		Model:    "gpt-4o-mini",
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("Count from 1 to 5")},
	})
	var streamed string
	for stream.Next() {
		if len(stream.Current().Choices) > 0 {
			piece := stream.Current().Choices[0].Delta.Content
			streamed += piece
			fmt.Print(piece)
		}
	}
	fmt.Println()
	if err := stream.Err(); err != nil || streamed == "" {
		err := stream.Err()
		if err == nil {
			err = fmt.Errorf("empty stream")
		}
		fail("stream", err)
	}

	weather, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model:    "gpt-4o-mini",
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("北京今天天气怎么样？")},
		Tools: []openai.ChatCompletionToolUnionParam{
			openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
				Name:        "get_weather",
				Description: openai.String("Current weather. Pass city exactly as the user wrote it."),
				Parameters: shared.FunctionParameters{
					"type":       "object",
					"properties": map[string]any{"city": map[string]any{"type": "string"}},
					"required":   []any{"city"},
				},
			}),
		},
	})
	if err != nil || len(weather.Choices[0].Message.ToolCalls) == 0 {
		if err == nil {
			err = fmt.Errorf("no tool call")
		}
		fail("tool", err)
	}
	call := weather.Choices[0].Message.ToolCalls[0]
	fmt.Printf("%s %s\n", call.Function.Name, call.Function.Arguments)

	js, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model:    "gpt-4o-mini",
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("What is the capital of France?")},
		ResponseFormat: openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{
				JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
					Name: "capital",
					Schema: map[string]any{
						"type":                 "object",
						"additionalProperties": false,
						"required":             []any{"city"},
						"properties":           map[string]any{"city": map[string]any{"type": "string"}},
					},
				},
			},
		},
	})
	if err != nil {
		fail("json", err)
	}
	var parsed map[string]any
	if json.Unmarshal([]byte(js.Choices[0].Message.Content), &parsed) != nil || len(parsed) == 0 {
		fail("json", fmt.Errorf("content %q", js.Choices[0].Message.Content))
	}
	fmt.Println(js.Choices[0].Message.Content)
}

// fail prints why a sample call did not return a usable body and exits 1.
func fail(step string, err error) {
	fmt.Fprintf(os.Stderr, "%s: %v\n", step, err)
	os.Exit(1)
}
