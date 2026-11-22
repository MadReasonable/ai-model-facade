// The Apply dialog's diff: its lines, applied to the running config, give
// the draft, and it marks no more lines changed than it must.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { diffLines } from '../../static/diff.mjs';

function apply(ops) {
	const before = ops.filter(([op]) => op !== '+').map(([, l]) => l);
	const after = ops.filter(([op]) => op !== '-').map(([, l]) => l);
	return { before, after, changed: ops.filter(([op]) => op !== ' ').length };
}

test('diffLines turns one list into the other', () => {
	const cases = [
		[[], []],
		[[], ['a']],
		[['a'], []],
		[['a', 'b', 'c'], ['a', 'b', 'c']],
		[['a', 'b', 'c', 'a', 'b', 'b', 'a'], ['c', 'b', 'a', 'b', 'a', 'c']],
		[['x', 'y'], ['y', 'x']],
	];
	for (const [a, b] of cases) {
		const r = apply(diffLines(a, b));
		assert.deepEqual(r.before, a);
		assert.deepEqual(r.after, b);
	}
});

test('diffLines finds the shortest edit', () => {
	// Myers' example: 5 changes is the least between these two
	assert.equal(apply(diffLines([...'ABCABBA'], [...'CBABAC'])).changed, 5);
	assert.equal(apply(diffLines(['a', 'b', 'c'], ['a', 'x', 'c'])).changed, 2);
});

test('diffLines on random edits', () => {
	let seed = 7;
	const rand = n => (seed = (seed * 1103515245 + 12345) % 2147483648) % n;
	for (let i = 0; i < 200; i++) {
		const a = Array.from({ length: rand(30) }, () => 'l' + rand(6));
		const b = a.filter(() => rand(4) > 0).map(l => (rand(5) === 0 ? 'new' + rand(3) : l));
		const r = apply(diffLines(a, b));
		assert.deepEqual(r.before, a);
		assert.deepEqual(r.after, b);
	}
});
