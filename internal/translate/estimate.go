package translate

import (
	"strings"

	"github.com/tidwall/gjson"
)

// charsPerToken is the usual rough size of a token in English text and code.
const charsPerToken = 4

// tokensPerImage stands in for an image or document, whose bytes say little
// about its tokens: base64 would count many times too high.
const tokensPerImage = 1000

// tokensPerMessage covers what a chat template adds around each message, the
// system prompt included, and tokensForReply the assistant turn it opens for
// the reply: ChatML, which Qwen models use, adds `<|im_start|>role\n` and
// `<|im_end|>\n`, 5 tokens, then `<|im_start|>assistant\n`, 3 (Qwen3-0.6B's
// tokenizer on LocalAI v4.11.0, counted 2026-11-10); Llama 3's template is
// within a token of both.
const (
	tokensPerMessage = 5
	tokensForReply   = 3
)

// EstimateMessagesTokens estimates a Messages body's input tokens, for a backend
// that can't count them. It can be well off, and it can't see what the backend
// adds, such as Open WebUI's tools and context.
func EstimateMessagesTokens(body []byte) int {
	texts, tokens := messagesInput(body)
	chars := 0
	for _, text := range texts {
		chars += len(text)
	}
	return tokens + chars/charsPerToken
}

// CountMessagesTokens counts a Messages body's input tokens with a model's
// tokenizer, for a backend that can tokenize text but not count a request:
// its text is counted exactly, in one call, and what the chat template adds
// and its images are estimated as EstimateMessagesTokens does.
func CountMessagesTokens(body []byte, tokenize func(text string) (int, error)) (int, error) {
	texts, tokens := messagesInput(body)
	counted, err := tokenize(strings.Join(texts, "\n"))
	if err != nil {
		return 0, err
	}
	return tokens + counted, nil
}

// messagesInput is a Messages body's input as its texts, the system prompt,
// tool definitions, messages and tool calls and results, and the tokens that
// stand in for what has no text: the chat template around each message and
// for the reply, and images and documents. Thinking is left out, since chat templates drop
// earlier turns' reasoning.
func messagesInput(body []byte) (texts []string, tokens int) {
	req := gjson.ParseBytes(body)
	add := func(text string) {
		if text != "" {
			texts = append(texts, text)
		}
	}
	tokens = tokensForReply
	if system := contentText(req.Get("system")); system != "" {
		tokens += tokensPerMessage
		add(system)
	}
	add(req.Get("tools").Raw)
	for _, message := range req.Get("messages").Array() {
		tokens += tokensPerMessage
		content := message.Get("content")
		if content.Type == gjson.String {
			add(content.String())
			continue
		}
		for _, block := range content.Array() {
			switch block.Get("type").String() {
			case "image", "document":
				tokens += tokensPerImage
			case "tool_use":
				add(block.Get("name").String())
				add(block.Get("input").Raw)
			case "tool_result":
				add(contentText(block.Get("content")))
			case "text":
				add(block.Get("text").String())
			}
		}
	}
	return texts, tokens
}

// contentText is the text of a string or of an array of text blocks.
func contentText(content gjson.Result) string {
	if content.Type == gjson.String {
		return content.String()
	}
	var texts []string
	for _, block := range content.Array() {
		texts = append(texts, block.Get("text").String())
	}
	return strings.Join(texts, "")
}
