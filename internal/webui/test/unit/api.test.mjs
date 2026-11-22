// The JSON the UI reads and writes keeps each object's key order, since
// the gateway adds a patch's keys to a request in config order.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { clone, configURL, deleteKey, keysOf, parseOrdered, prettyOrdered, setKey, stringifyOrdered, withOrder } from '../../static/api.mjs';

test('parseOrdered keeps integer-like keys where they were written', () => {
	const text = '{"b":1,"10":2,"a":{"2":true,"1":null},"list":[{"9":0,"x":1}]}';
	const v = parseOrdered(text);
	assert.deepEqual(keysOf(v), ['b', '10', 'a', 'list']);
	assert.deepEqual(keysOf(v.a), ['2', '1']);
	assert.equal(stringifyOrdered(v), text);
	assert.deepEqual(Object.keys(JSON.parse(text)), ['10', 'b', 'a', 'list'], 'JSON.parse moves integer-like keys first, the reason for parseOrdered');
});

test('parseOrdered reads what JSON.parse reads', () => {
	for (const text of ['0', '-1.5e3', '"a\\"\\\\\\u00e9"', 'true', 'null', '[]', '{}', ' [ 1 , {"a" : [ ] } ] ']) {
		assert.deepEqual(JSON.parse(stringifyOrdered(parseOrdered(text))), JSON.parse(text), text);
	}
});

test('parseOrdered refuses what isn\'t JSON', () => {
	for (const text of ['{', '{"a":1,}', '[1 2]', "{'a':1}", '01', 'tru', '{"a":1} x', '']) {
		assert.throws(() => parseOrdered(text), SyntaxError, text);
	}
});

test('setKey, deleteKey, withOrder and clone keep the order', () => {
	const v = parseOrdered('{"z":1,"5":2}');
	setKey(v, '1', 3);
	setKey(v, 'z', 4);
	assert.deepEqual(keysOf(v), ['z', '5', '1'], 'a new key is appended, an existing one stays in place');
	deleteKey(v, '5');
	assert.equal(stringifyOrdered(v), '{"z":4,"1":3}');
	assert.equal(stringifyOrdered(withOrder(v, ['1', 'z'])), '{"1":3,"z":4}');
	const c = clone(v);
	setKey(c, 'new', 0);
	assert.equal(stringifyOrdered(v), '{"z":4,"1":3}', 'a clone is a copy');
	assert.equal(stringifyOrdered(c), '{"z":4,"1":3,"new":0}');
});

test('prettyOrdered indents and parses back to the same value', () => {
	const v = parseOrdered('{"7":{"b":[1,{"2":"x"}],"a":{}},"e":[]}');
	const pretty = prettyOrdered(v);
	assert.equal(pretty, '{\n  "7": {\n    "b": [\n      1,\n      {\n        "2": "x"\n      }\n    ],\n    "a": {}\n  },\n  "e": []\n}');
	assert.equal(stringifyOrdered(parseOrdered(pretty)), stringifyOrdered(v));
});

test('configURL escapes each key, so a slash stays inside its key', () => {
	assert.equal(configURL([]), '/amf/config');
	assert.equal(configURL(['models', 'team/helper', 'a b']), '/amf/config/models/team%2Fhelper/a%20b');
});
