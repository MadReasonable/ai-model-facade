package anthropic

import (
	"encoding/json"
	"strings"

	"github.com/tidwall/gjson"
)

// finishReason maps a Messages stop_reason to Chat Completions'. A refusal
// is a content filter's decision in OpenAI's terms; pause_turn, which only
// server tools cause, and any reason added later end the reply as stop.
func finishReason(stop string) string {
	switch stop {
	case "max_tokens", "model_context_window_exceeded":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	}
	return "stop"
}

// usageOf converts Messages usage, read through usage, to Chat Completions'.
// Messages counts cache reads and writes apart from input_tokens; OpenAI's
// prompt_tokens includes them, with the reads again as cached_tokens.
func usageOf(usage func(path string) gjson.Result) map[string]any {
	cacheRead := usage("cache_read_input_tokens").Int()
	prompt := usage("input_tokens").Int() + usage("cache_creation_input_tokens").Int() + cacheRead
	completion := usage("output_tokens").Int()
	out := map[string]any{
		"prompt_tokens":         prompt,
		"completion_tokens":     completion,
		"total_tokens":          prompt + completion,
		"prompt_tokens_details": map[string]any{"cached_tokens": cacheRead},
	}
	if thinking := usage("output_tokens_details.thinking_tokens"); thinking.Exists() {
		out["completion_tokens_details"] = map[string]any{"reasoning_tokens": thinking.Int()}
	}
	return out
}

// completion turns a Messages response into a chat completion: text blocks
// joined as content, thinking as reasoning_content, as llama-server reports
// reasoning, and tool_use blocks as tool calls, each input as its arguments
// string. Other blocks, such as server tool results, have no place in a
// chat completion and are left out.
func completion(msg gjson.Result, created int64) []byte {
	var text, reasoning strings.Builder
	var calls []any
	for _, block := range msg.Get("content").Array() {
		switch block.Get("type").String() {
		case "text":
			text.WriteString(block.Get("text").String())
		case "thinking":
			reasoning.WriteString(block.Get("thinking").String())
		case "tool_use":
			calls = append(calls, map[string]any{
				"id":       block.Get("id").String(),
				"type":     "function",
				"function": map[string]any{"name": block.Get("name").String(), "arguments": block.Get("input").Raw},
			})
		}
	}
	reply := map[string]any{"role": "assistant", "content": text.String()}
	if text.Len() == 0 && len(calls) > 0 {
		reply["content"] = nil
	}
	if reasoning.Len() > 0 {
		reply["reasoning_content"] = reasoning.String()
	}
	if len(calls) > 0 {
		reply["tool_calls"] = calls
	}
	data, _ := json.Marshal(map[string]any{
		"id":      msg.Get("id").String(),
		"object":  "chat.completion",
		"created": created,
		"model":   msg.Get("model").String(),
		"choices": []any{map[string]any{
			"index":         0,
			"message":       reply,
			"finish_reason": finishReason(msg.Get("stop_reason").String()),
		}},
		"usage": usageOf(msg.Get("usage").Get),
	})
	return data
}

// openAIError turns a Claude API error body into OpenAI's error shape,
// keeping its message and type. A body that isn't one becomes the message.
func openAIError(body []byte) []byte {
	detail := gjson.GetBytes(body, "error")
	message, errType := detail.Get("message").String(), detail.Get("type").String()
	if message == "" {
		message, errType = strings.TrimSpace(string(body)), "api_error"
	}
	data, _ := json.Marshal(map[string]any{"error": map[string]any{"message": message, "type": errType}})
	return data
}
