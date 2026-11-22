// What each facade routes to: the tree's dots follow GET /amf/models?follow,
// so a load shows as it happens, with no listing read on a timer.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { chromePath, launch } from './chrome.mjs';
import { adminKey, fakeLlamaSwap, signIn, start } from './gateway.mjs';

const skip = !chromePath() && 'no Chrome on the PATH; set CHROME to one';

// dot is the class of the tree's dot for the row labelled label, run in
// the page; undefined while there is no such row.
const dot = label => [...document.querySelectorAll('.tree-row')].find(r => r.querySelector('.label')?.textContent === label)?.querySelector('.dot')?.className;

// dotIs is whether that class is want.
const dotIs = (label, want) => [...document.querySelectorAll('.tree-row')].find(r => r.querySelector('.label')?.textContent === label)?.querySelector('.dot')?.className === want;

test('a load the gateway starts shows in the tree as starting, then loaded, from the models stream', { skip }, async t => {
	const swap = await fakeLlamaSwap(['a', 'b', 'c']);
	const gw = await start({ swap: swap.url });
	const page = await launch();
	t.after(async () => {
		gw.stop();
		swap.close();
		await page.close();
	});
	await signIn(page, gw, '#/model/swap');
	await page.until(dotIs, 8000, 'b', 'dot ');
	await page.run(() => {
		window.reads = [];
		const fetch = window.fetch;
		window.fetch = (url, options) => {
			window.reads.push(String(url));
			return fetch(url, options);
		};
		return true;
	});

	const release = swap.hold();
	const asked = fetch(gw.url + '/v1/chat/completions', { method: 'POST', headers: { Authorization: 'Bearer ' + adminKey, 'Content-Type': 'application/json' }, body: '{"model": "swap", "messages": [{"role": "user", "content": "hi"}]}' });
	await page.until(dotIs, 4000, 'b', 'dot starting');
	release();
	assert.equal((await asked).status, 200);
	await page.until(dotIs, 4000, 'b', 'dot loaded');
	assert.equal(await page.run(dot, 'swap'), 'dot loaded', 'the facade, now routed to b');
	assert.deepEqual(await page.run(() => window.reads.filter(u => u.startsWith('/amf/models') && !u.includes('?follow'))), [], 'no listing read');
	assert.deepEqual(page.problems, []);
});
