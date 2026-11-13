package translate

import (
	"strings"
	"testing"
)

// The estimate counts text at four characters a token, a flat size for an image
// rather than its base64, and the template's tokens per message, the system
// prompt included, and for the reply.
func TestEstimate_CountsTextAndImages(t *testing.T) {
	text := strings.Repeat("a", 400)
	cases := map[string]int{
		`{"messages":[{"role":"user","content":"` + text + `"}]}`:                                       108,
		`{"system":"` + text + `","messages":[{"role":"user","content":"` + text + `"}]}`:               213,
		`{"messages":[{"role":"user","content":[{"type":"text","text":"` + text + `"}]}]}`:              108,
		`{"messages":[{"role":"user","content":[{"type":"image","source":{"data":"` + text + `"}}]}]}`:  1008,
		`{"messages":[{"role":"user","content":[{"type":"tool_result","content":"` + text + `"}]}]}`:    108,
		`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"` + text + `"}]}]}`: 8,
	}
	for body, want := range cases {
		if got := EstimateMessagesTokens([]byte(body)); got != want {
			t.Errorf("%.60s: got %d, want %d", body, got, want)
		}
	}
}

// A tokenizer counts the request's text in one call, in order, and the
// template's tokens around each message and an image's are estimated.
func TestCount_TokenizesTheTextAndEstimatesTheRest(t *testing.T) {
	body := `{"system":"be brief","tools":[{"name":"f"}],"messages":[` +
		`{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image","source":{}}]},` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"x"},{"type":"tool_use","name":"f","input":{"a":1}}]},` +
		`{"role":"user","content":[{"type":"tool_result","content":[{"type":"text","text":"ok"}]}]}]}`
	var sent string
	got, err := CountMessagesTokens([]byte(body), func(text string) (int, error) {
		sent = text
		return 10, nil
	})
	want := strings.Join([]string{"be brief", `[{"name":"f"}]`, "hi", "f", `{"a":1}`, "ok"}, "\n")
	if err != nil || got != 10+4*tokensPerMessage+tokensForReply+tokensPerImage || sent != want {
		t.Errorf("got %d, %v, sent %q", got, err, sent)
	}
}
