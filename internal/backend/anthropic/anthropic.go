// Package anthropic is the backend adapter for Anthropic's Claude API,
// through its Messages API. It accepts Messages and forwards it; a Chat
// Completions client reaches it through package translate, so params for it
// are always in Messages spelling.
//
// Model IDs come from GET /v1/models, read a page at a time, which needs an
// anthropic-version header, as /v1/messages does (verified: the API refuses
// either call without one, though its documentation lists it as optional).
// The adapter sends 2023-06-01; 2026-10-21 refuses temperature, top_p and
// top_k on every model, which Chat Completions clients send routinely. Each
// listed model's max_tokens is its output cap, which a request without
// max_tokens is given, since Messages requires one and a client on another
// protocol may not send it (verified: a non-streamed request at the full cap
// is accepted).
//
// A hosted model needs no load, so every listed model is ready while the API
// answers, and a state read answers from the latest list without calling the
// API; a failed request, or a list older than stateCacheSeconds, makes the
// next state read list the API again (openaihttp.ListedState).
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
	"github.com/MadReasonable/ai-model-facade/internal/backend/openaihttp"
	"github.com/MadReasonable/ai-model-facade/internal/config"
	"github.com/MadReasonable/ai-model-facade/internal/protocols"
)

// Backend sends chats to the Messages API through the embedded client's
// Send, translating them both ways.
type Backend struct {
	*openaihttp.Backend
	listed     *openaihttp.ListedState
	outputCaps atomic.Pointer[map[string]int64] // model -> max_tokens, from the latest list
}

// New builds the adapter from its config entry.
func New(entry config.Entry) (*Backend, error) {
	b := &Backend{}
	base, listed, err := openaihttp.NewListedBackend(entry, b.list)
	if err != nil {
		return nil, err
	}
	b.Backend, b.listed = base, listed
	return b, nil
}

// Protocols returns the one protocol the Claude API serves here.
func (b *Backend) Protocols() []backend.Protocol {
	return []backend.Protocol{backend.AnthropicMessages}
}

// Chat sends a Messages request, giving it the model's output cap if it has no
// max_tokens, and returns what the API answered, unchanged: a response, an
// error or a stream. A request with neither gets a 400 without being sent.
func (b *Backend) Chat(ctx context.Context, req backend.ChatRequest) (backend.ChatResult, error) {
	body, ok, err := withMaxTokens(req.Body, b.outputCap)
	if err != nil {
		return backend.ChatResult{}, err
	}
	if !ok {
		message := fmt.Sprintf("the Claude API needs max_tokens, and its model list gives no output cap for '%s' to use instead; set max_tokens", gjson.GetBytes(req.Body, "model").String())
		return backend.ChatResult{Response: &backend.ChatResponse{Status: http.StatusBadRequest, ContentType: "application/json", Body: errorBody("invalid_request_error", message)}}, nil
	}
	if body, err = withoutUnsignedThinking(body); err != nil {
		return backend.ChatResult{}, err
	}
	if body, err = withoutDisabledDisplay(body); err != nil {
		return backend.ChatResult{}, err
	}
	req.Body = body
	resp, err := b.Send(ctx, req, "/v1/messages", withMCPBeta(messagesHeader(req.Client), body))
	if err != nil {
		return backend.ChatResult{}, err
	}
	contentType := resp.Header.Get("Content-Type")
	mediaType, _, _ := mime.ParseMediaType(contentType)
	if req.Stream && resp.StatusCode/100 == 2 && mediaType == "text/event-stream" {
		return backend.ChatResult{Stream: protocols.Stream(backend.AnthropicMessages, resp.Body)}, nil
	}
	data, err := openaihttp.ReadBody(resp)
	if err != nil {
		return backend.ChatResult{}, err
	}
	return backend.ChatResult{Response: &backend.ChatResponse{Status: resp.StatusCode, ContentType: contentType, Body: data, RetryAfter: resp.Header.Get("Retry-After")}}, nil
}

// CountTokens asks the API, without the blocks it would refuse in a chat.
func (b *Backend) CountTokens(ctx context.Context, req backend.ChatRequest) (*backend.ChatResponse, error) {
	body, err := withoutUnsignedThinking(req.Body)
	if err != nil {
		return nil, err
	}
	if body, err = withoutDisabledDisplay(body); err != nil {
		return nil, err
	}
	req.Body = body
	return b.ForwardCountTokens(ctx, req, withMCPBeta(messagesHeader(req.Client), body))
}

// messagesHeader is the adapter's API version, with what the client asked for
// over it: a client's beta features and version are its own to choose.
func messagesHeader(client backend.Client) http.Header {
	header := apiVersion.Clone()
	for name, values := range client.MessagesHeader {
		header[name] = values
	}
	return header
}

// mcpBeta is the beta the MCP connector needs (Claude API docs; seen
// 2026-11-10).
const mcpBeta = "mcp-client-2025-11-20"

// withMCPBeta adds the MCP connector's beta to a request that names MCP
// servers, if the client didn't: a translated request's client, of another
// API, can't know Claude needs it.
func withMCPBeta(header http.Header, body []byte) http.Header {
	if !bytes.Contains(body, []byte(`"mcp_servers"`)) {
		return header
	}
	for _, value := range header.Values("Anthropic-Beta") {
		for _, beta := range strings.Split(value, ",") {
			if strings.HasPrefix(strings.TrimSpace(beta), "mcp-client-") {
				return header
			}
		}
	}
	header.Add("Anthropic-Beta", mcpBeta)
	return header
}

// withoutUnsignedThinking removes what the gateway made for a Messages client
// and only Claude can make: thinking blocks from a Chat Completions model's
// reasoning or an OpenAI reasoning item, which the API refuses (verified: a
// made-up signature is a 400 in any turn), and any assistant turn they leave
// empty; server tool calls and results made from an OpenAI API's records,
// whose IDs Claude didn't issue; and citations from an OpenAI API's, whose
// encrypted_index Claude can't read. What remains is a history it accepts, real signed blocks
// included (verified). A body without a marker passes on unchanged, with
// nothing parsed.
func withoutUnsignedThinking(body []byte) ([]byte, error) {
	if !bytes.Contains(body, []byte(backend.UnsignedThinking)) && !bytes.Contains(body, []byte(backend.OpenAIReasoning)) && !bytes.Contains(body, []byte(backend.GatewayCitation)) && !bytes.Contains(body, []byte(backend.GatewayServerTool)) {
		return body, nil
	}
	var messages []string
	removed := false
	gjson.GetBytes(body, "messages").ForEach(func(_, message gjson.Result) bool {
		content := message.Get("content")
		if !content.IsArray() {
			messages = append(messages, message.Raw)
			return true
		}
		var kept []string
		cited := false
		for _, block := range content.Array() {
			if block.Get("type").String() == "thinking" && backend.MadeByGateway(block.Get("signature").String()) {
				continue
			}
			if strings.HasPrefix(block.Get("id").String(), backend.GatewayServerTool) || strings.HasPrefix(block.Get("tool_use_id").String(), backend.GatewayServerTool) {
				continue
			}
			raw, changed := withoutGatewayCitations(block)
			cited = cited || changed
			kept = append(kept, raw)
		}
		if len(kept) == len(content.Array()) && !cited {
			messages = append(messages, message.Raw)
			return true
		}
		removed = true
		if len(kept) > 0 {
			edited, _ := sjson.SetRaw(message.Raw, "content", "["+strings.Join(kept, ",")+"]")
			messages = append(messages, edited)
		}
		return true
	})
	if !removed {
		return body, nil
	}
	return sjson.SetRawBytes(body, "messages", []byte("["+strings.Join(messages, ",")+"]"))
}

// withoutDisabledDisplay removes thinking.display when thinking is disabled,
// which the API refuses (verified on Claude Haiku 5.5: "thinking.disabled.display:
// Extra inputs are not permitted"). The Responses translator asks for display
// summarized when a client asks for a reasoning summary, and a facade's params,
// applied after it, can turn thinking off; with thinking off there is nothing to
// display, so the request is sent without it.
func withoutDisabledDisplay(body []byte) ([]byte, error) {
	thinking := gjson.GetBytes(body, "thinking")
	if thinking.Get("type").String() != "disabled" || !thinking.Get("display").Exists() {
		return body, nil
	}
	return sjson.DeleteBytes(body, "thinking.display")
}

// withoutGatewayCitations returns a text block without the citations the
// gateway made, and whether it had any.
func withoutGatewayCitations(block gjson.Result) (string, bool) {
	citations := block.Get("citations")
	if block.Get("type").String() != "text" || !citations.IsArray() {
		return block.Raw, false
	}
	var kept []string
	for _, c := range citations.Array() {
		if c.Get("encrypted_index").String() != backend.GatewayCitation {
			kept = append(kept, c.Raw)
		}
	}
	if len(kept) == len(citations.Array()) {
		return block.Raw, false
	}
	if len(kept) == 0 {
		raw, _ := sjson.Delete(block.Raw, "citations")
		return raw, true
	}
	raw, _ := sjson.SetRaw(block.Raw, "citations", "["+strings.Join(kept, ",")+"]")
	return raw, true
}

// withMaxTokens adds the model's output cap to a body with no max_tokens. ok
// is false if the model list gives no cap.
func withMaxTokens(body []byte, outputCap func(model string) (int64, bool)) (out []byte, ok bool, err error) {
	if v := gjson.GetBytes(body, "max_tokens"); v.Exists() && v.Type != gjson.Null {
		return body, true, nil
	}
	limit, ok := outputCap(gjson.GetBytes(body, "model").String())
	if !ok {
		return nil, false, nil
	}
	out, err = sjson.SetBytes(body, "max_tokens", limit)
	return out, err == nil, err
}

func errorBody(errType, message string) []byte {
	data, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]any{"type": errType, "message": message}})
	return data
}

// outputCap is model's max_tokens in the latest list.
func (b *Backend) outputCap(model string) (int64, bool) {
	caps := b.outputCaps.Load()
	if caps == nil {
		return 0, false
	}
	limit, ok := (*caps)[model]
	return limit, ok && limit > 0
}

// ListModels returns every model the API lists, and keeps them as its state.
func (b *Backend) ListModels(ctx context.Context) ([]backend.ModelInfo, error) {
	return b.listed.List(ctx)
}

// apiVersion is the Claude API version every call is made at; see the
// package comment for why not a later one.
var apiVersion = http.Header{"Anthropic-Version": {"2023-06-01"}}

// list reads every page of /v1/models, the largest the API allows, so most
// accounts need one call, and keeps each model's output cap.
func (b *Backend) list(ctx context.Context) ([]backend.ModelInfo, error) {
	var models []backend.ModelInfo
	caps := map[string]int64{}
	after := ""
	for {
		path := "/v1/models?limit=1000"
		if after != "" {
			path += "&after_id=" + url.QueryEscape(after)
		}
		var page struct {
			Data []struct {
				ID        string `json:"id"`
				MaxTokens int64  `json:"max_tokens"`
			} `json:"data"`
			HasMore bool   `json:"has_more"`
			LastID  string `json:"last_id"`
		}
		if err := b.GetJSONWith(ctx, path, apiVersion, &page); err != nil {
			return nil, err
		}
		for _, m := range page.Data {
			models = append(models, backend.ModelInfo{ID: m.ID, OutputCap: m.MaxTokens})
			caps[m.ID] = m.MaxTokens
		}
		if !page.HasMore || page.LastID == "" {
			b.outputCaps.Store(&caps)
			return models, nil
		}
		after = page.LastID
	}
}
