package openaihttp

import "testing"

// A rename keeps the key's place and every other byte, including a nested key
// of the same name; with both names, max_completion_tokens' value is the one
// that survives, whichever name stays.
func TestTokenLimit_RenamesInPlace(t *testing.T) {
	cases := []struct {
		name string
		use  func([]byte) ([]byte, error)
		body string
		want string
	}{
		{"to new, old only", UseMaxCompletionTokens, `{"a": 1.50, "max_tokens": 5,  "nested":{"max_tokens":9}}`, `{"a": 1.50, "max_completion_tokens": 5,  "nested":{"max_tokens":9}}`},
		{"to new, both", UseMaxCompletionTokens, `{"max_tokens":40,"max_completion_tokens":5,"a":1}`, `{"max_completion_tokens":5,"a":1}`},
		{"to new, new only", UseMaxCompletionTokens, `{"max_completion_tokens":5}`, `{"max_completion_tokens":5}`},
		{"to new, neither", UseMaxCompletionTokens, `{"a": 1}`, `{"a": 1}`},
		{"to old, new only", UseMaxTokens, `{"max_completion_tokens":5,"a":1}`, `{"max_tokens":5,"a":1}`},
		{"to old, both", UseMaxTokens, `{"max_tokens":40,"a":1,"max_completion_tokens":5}`, `{"max_tokens":5,"a":1}`},
		{"to old, old only", UseMaxTokens, `{"max_tokens":5}`, `{"max_tokens":5}`},
		{"to old, neither", UseMaxTokens, `{"a": 1}`, `{"a": 1}`},
	}
	for _, c := range cases {
		got, err := c.use([]byte(c.body))
		if err != nil || string(got) != c.want {
			t.Errorf("%s: got %s, %v; want %s", c.name, got, err, c.want)
		}
	}
}
