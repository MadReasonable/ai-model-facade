// The page hashes a key as the gateway does, so a key it makes or is given
// is sent as the hash server.apiKeys holds.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createHash, randomBytes } from 'node:crypto';
import { keyHash, sha256 } from '../../static/sha256.mjs';

test('sha256 gives the FIPS 180-4 test vectors', () => {
	assert.equal(sha256(''), 'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855');
	assert.equal(sha256('abc'), 'ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad');
	assert.equal(sha256('abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq'), '248d6a61d20638b8e5c026930c3e6039a33ce45964ff2167f6ecedd419db06c1');
	assert.equal(sha256('a'.repeat(1000000)), 'cdc76e5c9914fb9281a1c7e284d73e67f1809a48a497200e046d39ccc7112cd0');
});

test('sha256 agrees with Node\'s on every length across block edges, and on UTF-8', () => {
	for (let n = 0; n < 200; n++) {
		const text = randomBytes(n).toString('latin1') + (n % 9 === 0 ? 'é✓🔑' : '');
		assert.equal(sha256(text), createHash('sha256').update(text, 'utf8').digest('hex'), 'length ' + n);
	}
});

test('keyHash is the hash: line hash-key prints', () => {
	assert.equal(keyHash('abc'), 'sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad');
});
