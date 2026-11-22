// The editors color a line by these tokens; the tokens together are always
// the line, so the colored copy lines up with the text over it.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { jsonTokens, yamlTokens } from '../../static/highlight.mjs';

const kinds = tokens => tokens.filter(([k]) => k).map(([k, t]) => k + ':' + t);

test('yamlTokens finds keys, anchors, aliases, merges, values and comments', () => {
	for (const [line, want] of [
		['  noThinking: &noThinking # shared', ['key:noThinking', 'anchor:&noThinking', 'comment:# shared']],
		['    <<: *common', ['anchor:<<', 'anchor:*common']],
		['      - model: "*:Assistant" # quoted', ['key:model', 'string:"*:Assistant"', 'comment:# quoted']],
		['    apiKey: ${OPEN_WEBUI_KEY}', ['key:apiKey', 'env:${OPEN_WEBUI_KEY}']],
		['  stallTimeoutSeconds: 600', ['key:stallTimeoutSeconds', 'literal:600']],
		['  enabled: true', ['key:enabled', 'literal:true']],
		['  baseUrl: http://host:8080/v1#frag', ['key:baseUrl']],
		["  name: 'it''s # not a comment'", ['key:name', "string:'it''s # not a comment'"]],
		['# a comment line', ['comment:# a comment line']],
		['  - plain item', []],
	]) {
		const tokens = yamlTokens(line);
		assert.equal(tokens.map(([, t]) => t).join(''), line, 'the tokens are the line: ' + line);
		assert.deepEqual(kinds(tokens), want, line);
	}
});

test('jsonTokens tells a key from a string', () => {
	const line = '  "a": "b", "n": -1.5e3, "t": [true, null]';
	const tokens = jsonTokens(line);
	assert.equal(tokens.map(([, t]) => t).join(''), line);
	assert.deepEqual(kinds(tokens), ['key:"a"', 'string:"b"', 'key:"n"', 'literal:-1.5e3', 'key:"t"', 'literal:true', 'literal:null']);
});
