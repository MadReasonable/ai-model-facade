package virtualmodel

import (
	"fmt"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
	"github.com/MadReasonable/ai-model-facade/internal/mergepatch"
	"github.com/MadReasonable/ai-model-facade/internal/protocols"
)

// allProtocolsBlock names the params block applied whatever the protocol.
const allProtocolsBlock = "all"

// paramsBlocks are the keys that make a params map a set of blocks. Every
// protocol has a name no request field uses, so an old-style patch can't read
// as a block, and it keeps today's meaning.
var paramsBlocks = []backend.Protocol{backend.OpenAIChat, backend.OpenAICompletions, backend.AnthropicMessages}

// protocolParams is one `params` value. A setting is spelled differently in
// each protocol, so it holds patches for every protocol and patches for one.
type protocolParams struct {
	all    []mergepatch.Patch
	blocks map[backend.Protocol][]mergepatch.Patch
}

func (p protocolParams) empty() bool {
	return len(p.all) == 0 && len(p.blocks) == 0
}

func (p protocolParams) hasBlocks() bool {
	return len(p.blocks) > 0
}

// appendFor appends the patches for a body in protocol: `all`, then its block.
func (p protocolParams) appendFor(patches []mergepatch.Patch, protocol backend.Protocol) []mergepatch.Patch {
	return append(append(patches, p.all...), p.blocks[protocol]...)
}

// compileParams turns a params node into patches. A list, or a map with
// neither `all` nor a protocol name among its keys, is one old-style value,
// treated as `all`. A map that mixes the two is neither, so it is an error.
func compileParams(n *yaml.Node) (protocolParams, error) {
	if n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	if n.Kind != yaml.MappingNode || !hasBlockKey(n) {
		all, err := mergepatch.Compile(n)
		return protocolParams{all: all}, err
	}
	var params protocolParams
	for i := 0; i < len(n.Content); i += 2 {
		key, value := n.Content[i], n.Content[i+1]
		if !isBlockKey(key.Value) {
			return protocolParams{}, fmt.Errorf("line %d: params mixes protocol blocks with the key '%s'; put it in a block such as '%s'", key.Line, key.Value, allProtocolsBlock)
		}
		patches, err := mergepatch.Compile(value)
		if err != nil {
			return protocolParams{}, err
		}
		if key.Value == allProtocolsBlock {
			params.all = patches
			continue
		}
		if params.blocks == nil {
			params.blocks = map[backend.Protocol][]mergepatch.Patch{}
		}
		params.blocks[backend.Protocol(key.Value)] = patches
	}
	return params, nil
}

func hasBlockKey(n *yaml.Node) bool {
	for i := 0; i < len(n.Content); i += 2 {
		if isBlockKey(n.Content[i].Value) {
			return true
		}
	}
	return false
}

func isBlockKey(key string) bool {
	return key == allProtocolsBlock || slices.Contains(paramsBlocks, backend.Protocol(key))
}

// parseProtocols reads a facade's `protocols` list. None means every protocol.
func parseProtocols(names []string, line int) ([]backend.Protocol, error) {
	var parsed []backend.Protocol
	for _, name := range names {
		protocol := backend.Protocol(name)
		if !slices.Contains(protocols.Names(), protocol) {
			return nil, fmt.Errorf("line %d: unknown protocol '%s' in protocols; use one of %s", line, name, joinProtocols(protocols.Names()))
		}
		parsed = append(parsed, protocol)
	}
	return parsed, nil
}

func joinProtocols(protocols []backend.Protocol) string {
	names := make([]string, len(protocols))
	for i, p := range protocols {
		names[i] = string(p)
	}
	return strings.Join(names, ", ")
}
