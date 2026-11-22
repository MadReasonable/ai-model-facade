// The page remembers what it renamed, so the banner of names left behind
// can offer the new name.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { recordRename, renamedTo } from '../../static/renames.mjs';

test('a name typed a letter at a time is one rename', () => {
	recordRename('key', 'admin', 'r');
	recordRename('key', 'r', 'ro');
	recordRename('key', 'ro', 'root');
	assert.equal(renamedTo('key', 'admin'), 'root');
	assert.equal(renamedTo('key', 'r'), undefined);
	assert.equal(renamedTo('key', 'ro'), undefined);
});

test('a name renamed back is no rename', () => {
	recordRename('key', 'viewer', 'reader');
	recordRename('key', 'reader', 'viewer');
	assert.equal(renamedTo('key', 'viewer'), undefined);
});

test('keys and groups are renamed apart', () => {
	recordRename('group', 'family', 'household');
	assert.equal(renamedTo('group', 'family'), 'household');
	assert.equal(renamedTo('key', 'family'), undefined);
});
