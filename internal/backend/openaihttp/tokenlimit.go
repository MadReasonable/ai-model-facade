package openaihttp

import (
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Chat Completions has two names for the same limit, and no one name works on
// every backend. Verified 2026-11-08: OpenAI's models from gpt-5 on, and o3,
// refuse max_tokens even beside the other; Ollama 0.40.8 returns 200 for
// max_completion_tokens and ignores it, so nothing limits the reply; llama-swap
// v263, llama-server b11580, LM Studio and LocalAI honour either.
const (
	maxTokens           = "max_tokens"
	maxCompletionTokens = "max_completion_tokens"
)

// UseMaxCompletionTokens returns a chat body whose token limit is named
// max_completion_tokens, for a backend that refuses max_tokens.
func UseMaxCompletionTokens(body []byte) ([]byte, error) {
	return renameTokenLimit(body, maxTokens, maxCompletionTokens)
}

// UseMaxTokens returns a chat body whose token limit is named max_tokens, for a
// backend that ignores max_completion_tokens.
func UseMaxTokens(body []byte) ([]byte, error) {
	return renameTokenLimit(body, maxCompletionTokens, maxTokens)
}

// renameTokenLimit leaves one name, with max_completion_tokens' value if a body
// has both, as the backends that honour both do. A rename stays in place and
// touches no other byte, so key order and formatting survive. A body with
// neither passes through unchanged.
func renameTokenLimit(body []byte, from, to string) ([]byte, error) {
	source := gjson.GetBytes(body, from)
	if !source.Exists() {
		return body, nil
	}
	if gjson.GetBytes(body, to).Exists() {
		if to == maxCompletionTokens {
			return sjson.DeleteBytes(body, from)
		}
		out, err := sjson.SetRawBytes(body, to, []byte(source.Raw))
		if err != nil {
			return nil, err
		}
		return sjson.DeleteBytes(out, from)
	}
	out := body
	gjson.ParseBytes(body).ForEach(func(key, _ gjson.Result) bool {
		if key.String() != from {
			return true
		}
		out = append(append(append([]byte(nil), body[:key.Index]...), `"`+to+`"`...), body[key.Index+len(key.Raw):]...)
		return false
	})
	return out, nil
}
