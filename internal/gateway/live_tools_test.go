package gateway

import (
	"net/http"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// The live tool checks send each kind of tool across APIs to the real Claude
// and OpenAI APIs: server tools, which the vendor runs, and the tools a
// client runs to a vendor's schema. They need both keys, as the live
// Responses checks do, and the remote MCP check needs DeepWiki's public
// server. A server tool costs more than tokens, a search or a few seconds of
// a container, so these are the live checks that cost the most.

// liveToolFacades serve a Responses or Chat Completions client on Claude, and
// a Messages client on OpenAI through Responses, the two crossings every
// server tool has.
const liveToolFacades = `
  claude: {backend: claude, candidates: [{model: "${CLAUDE_MODEL}"}]}
  gpt: {backend: gpt, candidates: [{model: "${GPT_MODEL}", useProtocol: openAiResponses}]}
`

const liveMCPURL = "https://mcp.deepwiki.com/mcp"

// toolResponse asks facade on Claude, as a Responses client, with tools.
func toolResponse(t *testing.T, gw *gateway, tools, input string) gjson.Result {
	t.Helper()
	return gjson.Get(liveRespond(t, gw, `{"model":"claude","max_output_tokens":3000,"tools":`+tools+`,"input":`+input+`}`, false), "output")
}

// toolMessage asks facade on OpenAI, as a Messages client, with tools and any
// other fields, and returns the reply's content.
func toolMessage(t *testing.T, gw *gateway, fields, messages string) gjson.Result {
	t.Helper()
	r := messagesRequest(t, gw, liveMessagesPath, "", `{"model":"gpt","max_tokens":3000,`+fields+`,"messages":`+messages+`}`, nil)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("got %d %s", r.StatusCode, r.body)
	}
	return gjson.Get(r.body, "content")
}

func userTurn(text string) string {
	return `[{"role":"user","content":` + quoted(text) + `}]`
}

// Web search reaches Claude from a Responses or Chat Completions client and
// OpenAI from a Messages client, and its citations come back in the client's
// own shape. The Messages history then reaches Claude, which accepts only
// search records it made, so the gateway's records are removed on the way.
func TestLive_WebSearchCrossesAPIs(t *testing.T) {
	gw := startHostedGateway(t, liveToolFacades, "claude", "gpt")
	question := "Search the web for the current version of the Go programming language, and answer in one sentence."
	output := toolResponse(t, gw, `[{"type":"web_search"}]`, quoted(question))
	if !output.Get(`#(type=="web_search_call")`).Exists() || !output.Get(`#(type=="message").content.0.annotations.#(type=="url_citation")`).Exists() {
		t.Errorf("Responses on Claude: %s", output.Raw)
	}
	r := gw.chat(t, `{"model":"claude","max_completion_tokens":3000,"web_search_options":{},"messages":`+userTurn(question)+`}`)
	if r.StatusCode != http.StatusOK || !gjson.Get(r.body, `choices.0.message.annotations.#(type=="url_citation")`).Exists() {
		t.Errorf("Chat Completions on Claude: got %d %s", r.StatusCode, r.body)
	}
	content := toolMessage(t, gw, `"tools":[{"type":"web_search_20250305","name":"web_search"}]`, userTurn(question))
	if !content.Get(`#(type=="server_tool_use")`).Exists() || !content.Get(`#(type=="web_search_tool_result")`).Exists() || !content.Get(`#(type=="text").citations.0`).Exists() {
		t.Fatalf("Messages on OpenAI: %s", content.Raw)
	}
	history := `[{"role":"user","content":` + quoted(question) + `},{"role":"assistant","content":` + content.Raw + `},{"role":"user","content":"Say thanks in two words."}]`
	if r := messagesRequest(t, gw, liveMessagesPath, "", `{"model":"claude","max_tokens":200,"messages":`+history+`}`, nil); r.StatusCode != http.StatusOK {
		t.Errorf("the history on Claude: got %d %s", r.StatusCode, r.body)
	}
}

// Code execution reaches Claude from a Responses client and OpenAI from a
// Messages client, and the record of each run comes back in the client's API.
func TestLive_CodeExecutionCrossesAPIs(t *testing.T) {
	gw := startHostedGateway(t, liveToolFacades, "claude", "gpt")
	question := "Run Python code to compute 2**100 % 97, and answer with the number."
	output := toolResponse(t, gw, `[{"type":"code_interpreter","container":{"type":"auto"}}]`, quoted(question))
	if !output.Get(`#(type=="code_interpreter_call")`).Exists() {
		t.Errorf("Responses on Claude: %s", output.Raw)
	}
	content := toolMessage(t, gw, `"tools":[{"type":"code_execution_20260120","name":"code_execution"}]`, userTurn(question))
	if !content.Get(`#(type=="server_tool_use")`).Exists() {
		t.Errorf("Messages on OpenAI: %s", content.Raw)
	}
}

// A remote MCP server reaches Claude from a Responses client and OpenAI from a
// Messages client, and each call comes back in the client's API.
func TestLive_MCPCrossesAPIs(t *testing.T) {
	gw := startHostedGateway(t, liveToolFacades, "claude", "gpt")
	question := "Use the deepwiki tools to say in one sentence what the GitHub repository golang/go is."
	output := toolResponse(t, gw, `[{"type":"mcp","server_label":"deepwiki","server_url":"`+liveMCPURL+`","require_approval":"never"}]`, quoted(question))
	if !output.Get(`#(type=="mcp_call")`).Exists() {
		t.Errorf("Responses on Claude: %s", output.Raw)
	}
	content := toolMessage(t, gw, `"mcp_servers":[{"type":"url","url":"`+liveMCPURL+`","name":"deepwiki"}],"tools":[{"type":"mcp_toolset","mcp_server_name":"deepwiki"}]`, userTurn(question))
	if !content.Get(`#(type=="mcp_tool_use")`).Exists() {
		t.Errorf("Messages on OpenAI: %s", content.Raw)
	}
}

// OpenAI's shell reaches Claude as its bash tool, and Claude's bash reaches
// OpenAI as its shell, each call coming back as the client's own tool's, and
// its output going back in a complete loop.
func TestLive_ShellCrossesAPIs(t *testing.T) {
	gw := startHostedGateway(t, liveToolFacades, "claude", "gpt")
	question := "Use the shell to print the current directory, then tell me what it is."
	input := userTurn(question)
	output := toolResponse(t, gw, `[{"type":"shell"}]`, input)
	call := output.Get(`#(type=="shell_call")`)
	if len(call.Get("action.commands").Array()) == 0 {
		t.Fatalf("Responses on Claude: %s", output.Raw)
	}
	result := `{"type":"shell_call_output","call_id":` + quoted(call.Get("call_id").String()) + `,"output":[{"stdout":"/home/user\n","stderr":"","outcome":{"type":"exit","exit_code":0}}]}`
	toolResponse(t, gw, `[{"type":"shell"}]`, strings.TrimSuffix(input, "]")+","+call.Raw+","+result+"]")

	tools := `"tools":[{"type":"bash_20250124","name":"bash"}]`
	content := toolMessage(t, gw, tools, input)
	use := content.Get(`#(type=="tool_use")`)
	if use.Get("name").String() != "bash" || use.Get("input.command").String() == "" {
		t.Fatalf("Messages on OpenAI: %s", content.Raw)
	}
	history := `[{"role":"user","content":` + quoted(question) + `},{"role":"assistant","content":` + content.Raw + `},{"role":"user","content":[{"type":"tool_result","tool_use_id":` + quoted(use.Get("id").String()) + `,"content":"/home/user"}]}]`
	toolMessage(t, gw, tools, history)
}

// File editing crosses as functions: OpenAI's apply_patch reaches Claude as a
// tool that takes a patch, and Claude's text editor reaches OpenAI as a
// function with the editor's schema, each call coming back as the client's
// own tool's.
func TestLive_FileEditingCrossesAPIs(t *testing.T) {
	gw := startHostedGateway(t, liveToolFacades, "claude", "gpt")
	question := "Create the file hello.txt containing the line Hello, world."
	output := toolResponse(t, gw, `[{"type":"apply_patch"}]`, userTurn(question))
	if operation := output.Get(`#(type=="apply_patch_call").operation`); operation.Get("path").String() == "" || operation.Get("diff").String() == "" {
		t.Errorf("Responses on Claude: %s", output.Raw)
	}
	content := toolMessage(t, gw, `"tools":[{"type":"text_editor_20250728","name":"str_replace_based_edit_tool"}]`, userTurn(question))
	if use := content.Get(`#(type=="tool_use")`); use.Get("name").String() != "str_replace_based_edit_tool" || use.Get("input.command").String() == "" {
		t.Errorf("Messages on OpenAI: %s", content.Raw)
	}
}

// A namespace tool, as Codex groups its sub-agent functions in, reaches
// Claude as functions named for their namespace, and a call comes back with
// its namespace and its own name, which Claude takes back in a complete loop.
func TestLive_NamespacesReachClaude(t *testing.T) {
	gw := startHostedGateway(t, liveToolFacades, "claude")
	tools := `[{"type":"namespace","name":"weather","description":"Weather tools.","tools":` + liveResponsesTools + `}]`
	input := userTurn(liveToolQuestion)
	output := toolResponse(t, gw, tools, input)
	call := output.Get(`#(type=="function_call")`)
	if call.Get("namespace").String() != "weather" || call.Get("name").String() != "get_weather" {
		t.Fatalf("got %s", output.Raw)
	}
	result := `{"type":"function_call_output","call_id":` + quoted(call.Get("call_id").String()) + `,"output":"Paris: 14C, cloudy."}`
	toolResponse(t, gw, tools, strings.TrimSuffix(input, "]")+","+call.Raw+","+result+"]")
}
