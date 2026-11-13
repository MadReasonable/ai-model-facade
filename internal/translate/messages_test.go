package translate

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// A Messages history with a search result and a cited document, as it
// reaches each OpenAI API: the search result becomes text, and what has no
// counterpart is reported: document citations, Claude's service settings.
// Claude's max effort becomes OpenAI's highest, xhigh.
func TestMessages_TranslatesHistoryAndSettingsForEitherOpenAIAPI(t *testing.T) {
	body := `{"output_config":{"effort":"max"},"speed":"fast","diagnostics":{},"user_profile_id":"u","workspace_id":"w","messages":[` +
		`{"role":"user","content":[{"type":"search_result","title":"Oslo","source":"https://x","content":[{"type":"text","text":"Big city."}]},{"type":"document","citations":{"enabled":true},"source":{"type":"text","data":"doc"}}]},` +
		`{"role":"assistant","content":[{"type":"text","text":"Found."}]}]}`
	cases := []struct {
		name       string
		translator Translator
		effort     string
		texts      string
	}{
		{"chat", messagesToChat{}, "reasoning_effort", "messages.#.content"},
		{"responses", messagesToResponses{}, "reasoning.effort", "input.#.content"},
	}
	for _, c := range cases {
		out, reply, err := c.translator.Request([]byte(body))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if gjson.GetBytes(out, c.effort).String() != "xhigh" {
			t.Errorf("%s: effort in %s", c.name, out)
		}
		var all []string
		for _, content := range gjson.GetBytes(out, c.texts).Array() {
			all = append(all, content.String())
			for _, part := range content.Array() {
				all = append(all, part.Get("text").String())
			}
		}
		texts := strings.Join(all, "|")
		for _, want := range []string{"Oslo (https://x)", "Big city.", "Found."} {
			if !strings.Contains(texts, want) {
				t.Errorf("%s: %q missing from %s", c.name, want, texts)
			}
		}
		dropped := strings.Join(reply.Dropped(), ",")
		for _, want := range []string{"speed", "diagnostics", "user_profile_id", "workspace_id", "citations"} {
			if !strings.Contains(dropped, want) {
				t.Errorf("%s: %s not reported in %s", c.name, want, dropped)
			}
		}
	}
}

// A document by URL reaches Responses as a file by URL; Chat Completions has
// no way to take one.
func TestMessages_SendsADocumentByURLWhereItCan(t *testing.T) {
	body := `{"messages":[{"role":"user","content":[{"type":"document","source":{"type":"url","url":"https://x/d.pdf"}}]}]}`
	out, _, err := messagesToResponses{}.Request([]byte(body))
	if err != nil || gjson.GetBytes(out, "input.0.content.0").Raw != `{"type":"input_file","file_url":"https://x/d.pdf"}` {
		t.Errorf("responses: %s, %v", out, err)
	}
	if _, _, err := (messagesToChat{}).Request([]byte(body)); err == nil {
		t.Error("chat: a document by URL was taken")
	}
}
