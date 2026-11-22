// Editing the draft: when a field's change is sent, what Apply takes, and
// that the files keep their aliases and comments through it all.
import { once } from 'node:events';
import { createServer } from 'node:http';
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { chromePath, launch } from './chrome.mjs';
import { adminKey, signIn, start } from './gateway.mjs';

const skip = !chromePath() && 'no Chrome on the PATH; set CHROME to one';
const sleep = ms => new Promise(r => setTimeout(r, ms));

async function session(t, options) {
	const gw = await start();
	const page = await launch(options);
	t.after(async () => {
		gw.stop();
		await page.close();
	});
	return { gw, page };
}

const field = name => `() => [...document.querySelectorAll('.field')].find(f => f.querySelector('.name').textContent.startsWith('${name}')).querySelector('input')`;

test('a field is sent after a pause, and Apply sends one still unsent first', { skip }, async t => {
	const { gw, page } = await session(t);
	await signIn(page, gw, '#/model/swap');
	await page.mark(eval(field('description')), 'desc');
	await page.type('#desc', 'Paused');
	await sleep(200);
	assert.equal((await gw.api('/amf/config/models/swap')).description, undefined, 'not while typing');
	await sleep(700);
	assert.equal((await gw.api('/amf/config/models/swap')).description, 'Paused', 'after the pause');
	assert.equal(await page.run(() => document.activeElement.id), 'desc', 'and the field keeps the focus');
	await page.until(() => document.querySelector('.topbar').textContent.includes('changes staged'));

	await page.type('#desc', 'Applied at once');
	await page.click('.topbar button', 'Apply');
	await page.until(() => document.querySelector('.dialog .diff')?.textContent.includes('Applied at once'));
	await page.click('.dialog .btn.primary', 'Apply');
	await page.until(() => document.querySelector('.topbar').textContent.includes('config live'));
	assert.equal(await gw.api('/amf/config/models/swap/description?live'), 'Applied at once');
	assert.deepEqual(page.problems, []);
});

test('Test sends an unsent field first, and its result stays, marked at the same height while the address differs', { skip }, async t => {
	const { gw, page } = await session(t);
	const seen = [];
	const backend = createServer((req, res) => {
		seen.push(req.method + ' ' + req.url);
		res.setHeader('Content-Type', 'application/json');
		res.end(JSON.stringify({ data: [{ id: 'a' }, { id: 'b' }] }));
	}).listen(0, '127.0.0.1');
	await once(backend, 'listening');
	t.after(() => backend.close());
	await signIn(page, gw, '#/backend/local');

	await page.click('.head-row button', 'Test');
	await page.until(() => document.querySelector('.card .banner.bad')?.textContent.includes('127.0.0.1:1'));

	await page.mark(eval(field('baseUrl')), 'url');
	await page.type('#url', 'http://127.0.0.1:' + backend.address().port);
	await page.click('.head-row button', 'Test');
	await page.until(() => document.querySelector('.card .banner.good')?.textContent.includes('lists 2 models'));
	assert.deepEqual(seen, ['GET /v1/models']);
	assert.equal((await gw.api('/amf/config/backends/local')).baseUrl, 'http://127.0.0.1:' + backend.address().port, 'the field reached the draft first');
	const height = await page.run(() => document.querySelector('.card .banner').offsetHeight);

	await page.type('#url', 'http://127.0.0.1:2', { enter: true });
	await page.until(() => document.querySelector('.card .banner.warn'));
	assert.equal(await page.run(() => document.querySelector('.card .banner').textContent.trim()), 'baseUrl has changed since this test; test again.');
	assert.equal(await page.run(() => document.querySelector('.card .banner').offsetHeight), height, 'the box keeps its height');

	await page.type('#url', 'http://127.0.0.1:' + backend.address().port, { enter: true });
	await page.until(() => document.querySelector('.card .banner.good')?.textContent.includes('lists 2 models'));
	assert.deepEqual(page.problems, []);
});

test('reorder, add and rename keep the file\'s aliases, and save backs up first', { skip }, async t => {
	const { gw, page } = await session(t);
	await signIn(page, gw, '#/model/swap');
	await page.click('table.list tr.row:nth-child(1) button[title="Move down"]');
	await sleep(500);
	assert.deepEqual((await gw.api('/amf/config/models/swap/candidates')).map(c => c.model), ['b', 'a']);
	await page.click('.card h2 button', 'Add a candidate');
	await page.type('.dialog input[type=text]', 'c');
	await page.click('.dialog .btn.primary', 'Add');
	await sleep(500);
	assert.deepEqual((await gw.api('/amf/config/models/swap/candidates')).map(c => c.model), ['b', 'a', 'c']);

	await page.run(() => (location.hash = '#/model/single'));
	await page.until(() => document.querySelector('.head-row h1')?.textContent.startsWith('single'));
	await page.click('.head-row button', 'Rename');
	await page.type('.dialog input', 'solo');
	await page.click('.dialog .btn.primary', 'OK');
	await page.until(() => location.hash === '#/model/solo');

	await page.click('.topbar button', 'Apply');
	await page.until(() => document.querySelector('.dialog .diff'));
	await page.click('.dialog .btn.primary', 'Apply');
	await page.until(() => document.querySelector('.topbar').textContent.includes('config live'));
	await page.click('.topbar button', 'Save');
	await page.click('.dialog .btn.primary', 'Save');
	await page.until(() => [...document.querySelectorAll('.topbar .pill')].some(p => p.textContent === 'saved') && !document.querySelector('.dialog'));
	const file = gw.read('config');
	assert.match(file, /\n {2}solo:\n {4}backend: local\n {4}params: \*noThinking\n/, 'renamed in place, alias kept');
	assert.match(file, /\n {2}swap:\n {4}backend: local\n {4}params: \*noThinking\n/);
	assert.match(file, /baseUrl: http:\/\/127\.0\.0\.1:1 # nothing listens here/, 'comments kept');
	assert.deepEqual(page.problems, []);
});

test('an edit of vars reaches every value that uses the anchor', { skip }, async t => {
	const { gw, page } = await session(t);
	await signIn(page, gw, '#/model/swap?vars');
	await page.until(() => document.querySelector('textarea.code')?.value.includes('noThinking'));
	const yaml = await page.run(() => document.querySelector('textarea.code').value);
	await page.run(text => {
		const el = document.querySelector('textarea.code');
		Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value').set.call(el, text);
		el.dispatchEvent(new Event('input', { bubbles: true }));
	}, yaml.replace('enable_thinking: false}', 'enable_thinking: false}\n  temperature: 0.5'));
	await page.click('button', 'Save to the draft');
	await sleep(600);
	assert.equal((await gw.api('/amf/config/models/single/params')).temperature, 0.5);
	assert.equal(await page.run(() => document.querySelector('.anchor-list').getBoundingClientRect().x > document.querySelector('.editor textarea').getBoundingClientRect().right), true, 'the anchors are on the right');
	assert.deepEqual(page.problems, []);
});

test('back and forward step through what was shown', { skip }, async t => {
	const { gw, page } = await session(t);
	await signIn(page, gw, '#/server');
	await page.click('.tree-row', 'local');
	await page.until(() => location.hash === '#/backend/local');
	await page.click('.tree-row', 'swap');
	await page.until(() => location.hash === '#/model/swap');
	await page.run(() => history.back());
	await page.until(() => document.querySelector('.head-row h1')?.textContent.startsWith('local'));
	await page.run(() => history.forward());
	await page.until(() => document.querySelector('.head-row h1')?.textContent.startsWith('swap'));
	assert.deepEqual(page.problems, []);
});

test('another admin\'s edit shows from the status stream, which leaves the draft unread until it changes', { skip }, async t => {
	const { gw, page } = await session(t);
	await signIn(page, gw, '#/model/swap');
	await page.until(() => document.querySelector('.topbar').textContent.includes('config live'));
	await page.run(() => {
		window.reads = [];
		const fetch = window.fetch;
		window.fetch = (url, options) => {
			window.reads.push(String(url));
			return fetch(url, options);
		};
		return true;
	});
	await sleep(6000); // longer than the 5 s the page used to read the draft at
	assert.deepEqual(await page.run(() => window.reads.filter(u => u.startsWith('/amf/config') && !u.startsWith('/amf/config-status?follow'))), [], 'nothing read while nothing changes');

	const edit = await fetch(gw.url + '/amf/config/models/swap/description', { method: 'PUT', headers: { Authorization: 'Bearer ' + adminKey, 'Content-Type': 'application/json' }, body: '"Another admin"' });
	assert.equal(edit.status, 201); // swap has no description yet
	await page.until(() => document.querySelector('.topbar').textContent.includes('changes staged'), 1000);
	assert.ok((await page.run(() => window.reads)).includes('/amf/config'), 'the draft is read once it changed');
	assert.ok(!(await page.run(() => window.reads)).includes('/amf/config-status'), 'the status comes from the stream, not read again');
	assert.deepEqual(page.problems, []);
});
