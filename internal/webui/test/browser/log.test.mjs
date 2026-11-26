// Where the log panel goes on a pick of a sheet made while the sheet may be
// hidden, from the tree, the bar or the history: a log that fills the pane
// moves below the sheet, so the sheet shows, and on a phone, where the log
// covers the screen, the log closes.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { chromePath, launch } from './chrome.mjs';
import { signIn, start } from './gateway.mjs';

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

test('a pick from the tree moves a log that fills the pane below the sheet', { skip }, async t => {
	const { gw, page } = await session(t);
	await signIn(page, gw, '#/server');
	await page.click('.topbar button', 'Log');
	await page.click('.log-bar .seg button[title="The entire pane"]');
	await page.until(() => document.querySelector('.main.log-full'));
	await page.click('.tree-row', 'swap');
	await page.until(() => location.hash === '#/model/swap' && !document.querySelector('.main.log-full'));
	assert.equal(await page.run(() => document.querySelector('.log-bar .seg button.on').title), 'Below the sheet');
	assert.deepEqual(page.problems, []);
});

// The page is short enough that the log's default height is more than 40%
// of the pane, so the test sees it shrink.
test('Load a backup... moves a log that fills the pane below the sheet, at most 40% of the pane', { skip }, async t => {
	const { gw, page } = await session(t, { width: 1280, height: 600 });
	await signIn(page, gw, '#/model/swap');
	await page.click('.topbar button', 'Log');
	await page.click('.log-bar .seg button[title="The entire pane"]');
	await page.until(() => document.querySelector('.main.log-full'));
	await page.click('button[title="More"]');
	await page.click('.btn.ghost', 'Load a backup...');
	await page.until(() => location.hash === '#/server' && !document.querySelector('.main.log-full'));
	await sleep(500);
	const [log, pane, card, content] = JSON.parse(await page.run(() => JSON.stringify([
		document.querySelector('.log').offsetHeight,
		document.querySelector('.main').clientHeight,
		Math.round(document.getElementById('backups').getBoundingClientRect().top),
		document.querySelector('.main .content').getBoundingClientRect().bottom,
	])));
	assert.ok(log <= Math.round(pane * 0.4), `log ${log}px of a ${pane}px pane`);
	assert.ok(card < content, `the backups card starts at ${card}, below the sheet's bottom at ${content}`);
	assert.deepEqual(page.problems, []);
});

test('back moves a log that fills the pane below the sheet', { skip }, async t => {
	const { gw, page } = await session(t);
	await signIn(page, gw, '#/server');
	await page.click('.tree-row', 'swap');
	await page.until(() => location.hash === '#/model/swap');
	await page.click('.topbar button', 'Log');
	await page.click('.log-bar .seg button[title="The entire pane"]');
	await page.until(() => document.querySelector('.main.log-full'));
	await page.run(() => history.back());
	await page.until(() => location.hash === '#/server' && !document.querySelector('.main.log-full'));
	assert.deepEqual(page.problems, []);
});

test('on a phone, a pick from the tree closes the log, which covers the screen', { skip }, async t => {
	const { gw, page } = await session(t, { width: 400, height: 820 });
	await signIn(page, gw, '#/server');
	await page.click('.topbar button', 'Log');
	await page.until(() => document.querySelector('.main.log-full'));
	await page.click('.nav-toggle');
	await page.until(() => document.querySelector('.tree-pane').getBoundingClientRect().x === 0);
	await page.click('.tree-row', 'swap');
	await page.until(() => location.hash === '#/model/swap' && !document.querySelector('.log'));
	assert.deepEqual(page.problems, []);
});

// The drag sets the height as it moves and remembers only the height it
// ends at, which the next load starts with.
test('a drag resizes a docked log, and the next load keeps the height it ended at', { skip }, async t => {
	const { gw, page } = await session(t);
	await signIn(page, gw, '#/server');
	await page.click('.topbar button', 'Log');
	await page.until(() => document.querySelector('.log-resize'));
	const start = await page.run(() => document.querySelector('.log').getBoundingClientRect().height);
	const moved = await page.run(() => {
		const handle = document.querySelector('.log-resize');
		const y = handle.getBoundingClientRect().top;
		handle.dispatchEvent(new PointerEvent('pointerdown', { clientY: y, bubbles: true }));
		dispatchEvent(new PointerEvent('pointermove', { clientY: y - 100 }));
		return localStorage.getItem('amf-log-height');
	});
	assert.equal(moved, null, 'a move remembers no height');
	await page.until(h => document.querySelector('.log').getBoundingClientRect().height === h, 8000, start + 100);
	await page.run(() => {
		const y = document.querySelector('.log-resize').getBoundingClientRect().top;
		dispatchEvent(new PointerEvent('pointerup', { clientY: y }));
	});
	assert.equal(await page.run(() => localStorage.getItem('amf-log-height')), String(start + 100));
	await page.run(() => location.reload());
	await page.until(h => document.querySelector('.log')?.getBoundingClientRect().height === h, 8000, start + 100);
	assert.deepEqual(page.problems, []);
});
