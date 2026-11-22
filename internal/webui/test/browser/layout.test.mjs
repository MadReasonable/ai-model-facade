// The page's layout: the head stays put across views, and on a phone the
// tree is a drawer, the log takes the screen, and the bar's buttons and the
// sheet's name row move out of the way as the sheet scrolls.
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

test('the view buttons and the title stay put across Properties, YAML and &vars', { skip }, async t => {
	const { gw, page } = await session(t);
	await signIn(page, gw, '#/model/swap');
	const at = () => page.run(() => JSON.stringify(['.head-row .seg', '.head-row h1'].map(s => {
		const r = document.querySelector(s).getBoundingClientRect();
		return [Math.round(r.x), Math.round(r.y)];
	})));
	const properties = await at();
	for (const view of ['YAML', '&vars']) {
		await page.click('.head-row .seg button', view);
		await sleep(300);
		assert.equal(await at(), properties, view);
	}
	assert.deepEqual(page.problems, []);
});

test('the fold button folds the facades, then the sections, then opens everything', { skip }, async t => {
	const { gw, page } = await session(t);
	await signIn(page, gw);
	const rows = () => page.run(() => [...document.querySelectorAll('.tree-row .label')].map(l => l.textContent.trim()));
	const all = (await rows()).length;
	await page.click('.tree-fold');
	assert.equal((await rows()).includes('a'), false, 'candidates folded');
	await page.click('.tree-fold');
	assert.deepEqual(await rows(), ['Server', 'Backends', 'Models']);
	await page.click('.tree-fold');
	assert.equal((await rows()).length, all);
});

test('on a phone, the tree is a drawer, the log takes the screen, and the bar moves with the scroll', { skip }, async t => {
	const { gw, page } = await session(t, { width: 400, height: 820 });
	await signIn(page, gw, '#/server');
	assert.equal(await page.run(() => getComputedStyle(document.querySelector('.tree-pane')).visibility), 'hidden');
	await page.click('.nav-toggle');
	await page.until(() => document.querySelector('.tree-pane').getBoundingClientRect().x === 0);
	await page.click('.tree-row', 'swap');
	await page.until(() => !document.querySelector('.drawer-backdrop') && location.hash === '#/model/swap');

	await page.run(() => (location.hash = '#/server'));
	await page.until(() => document.querySelector('.head-row h1')?.textContent === 'Server');
	const where = () => page.run(() => {
		const r = s => Math.round(document.querySelector(s).getBoundingClientRect().bottom);
		return { row: r('.bar-row'), bar: r('.topbar'), buttons: r('.actions-wrap'), name: r('.head-row') };
	});
	const scroll = y => page.run(y => (document.querySelector('.main .content').scrollTop = y), y);
	const top = await where();
	await scroll(30);
	await sleep(100);
	let now = await where();
	assert.equal(now.row, top.row, 'the first row stays');
	assert.equal(now.buttons, top.buttons - 30, 'the buttons move with the scroll');
	await scroll(900);
	await sleep(100);
	now = await where();
	assert.ok(now.name <= now.bar + 1, 'the name row is under the bar, deep down: ' + JSON.stringify(now));
	await scroll(870);
	await sleep(100);
	assert.equal((await where()).name, now.name + 30, 'and comes back at the speed of the scroll');

	await page.click('.topbar button', 'Log');
	await page.until(() => document.querySelector('.main.log-full'));
	assert.equal(await page.run(() => !!document.querySelector('.log-bar .seg')), false, 'no docked mode on a phone');
	assert.deepEqual(page.problems, []);
});

for (const [device, width] of [['a desktop', 1280], ['a phone', 400]]) {
	test(`on ${device}, Load a backup... shows Files and backups below the bar`, { skip }, async t => {
		const { gw, page } = await session(t, { width, height: 820 });
		await signIn(page, gw, '#/model/swap');
		await page.click('button[title="More"]');
		await page.click('.btn.ghost', 'Load a backup...');
		await sleep(500);
		const [hash, card, bar] = JSON.parse(await page.run(() => JSON.stringify([
			location.hash,
			Math.round(document.getElementById('backups').getBoundingClientRect().top),
			Math.round(document.querySelector('.topbar').getBoundingClientRect().bottom),
		])));
		assert.equal(hash, '#/server');
		assert.ok(Math.abs(card - bar - 12) <= 1, `card at ${card}, bar ends at ${bar}; want 12px between`);
		assert.deepEqual(page.problems, []);
	});
}
