// Signing in: the key the page signs in with, what a reader may do, and how
// the key is kept and forgotten.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { chromePath, launch } from './chrome.mjs';
import { readerKey, signIn, start } from './gateway.mjs';

const skip = !chromePath() && 'no Chrome on the PATH; set CHROME to one';

async function session(t, options) {
	const gw = await start();
	const page = await launch(options);
	t.after(async () => {
		gw.stop();
		await page.close();
	});
	return { gw, page };
}

test('a wrong key is refused, and a reader sees every control off', { skip }, async t => {
	const { gw, page } = await session(t);
	await page.goto(gw.url + '/amf/ui/#/model/swap');
	await page.until(() => document.querySelector('.signin form'));
	await page.type('input[name=password]', 'not-a-key');
	await page.click('.signin button', 'Sign in');
	await page.until(() => document.querySelector('.signin .banner'));
	assert.match(await page.run(() => document.querySelector('.signin .banner').textContent), /doesn't accept this key/);

	await page.type('input[name=password]', readerKey);
	await page.click('.signin button', 'Sign in');
	await page.until(() => document.querySelector('.app .head-row h1')?.textContent.startsWith('swap'));
	assert.equal(await page.run(() => document.querySelector('.topbar').textContent.includes('Apply')), false);
	assert.equal(await page.run(() => document.querySelector('.topbar').textContent.includes('read only')), true);
	assert.equal(await page.run(() => [...document.querySelectorAll('.sheet input, .sheet select')].every(e => e.disabled || e.type === 'search')), true);
	assert.deepEqual(page.problems, []);
});

test('the key shows on its toggle, stays through a reload when asked, and sign-out forgets it', { skip }, async t => {
	const { gw, page } = await session(t);
	await page.goto(gw.url + '/amf/ui/');
	await page.until(() => document.querySelector('.signin form'));
	await page.type('input[name=password]', 'admin-key');
	assert.equal(await page.run(() => document.querySelector('input[name=password]').type), 'password');
	await page.click('.signin .reveal-toggle');
	assert.equal(await page.run(() => document.querySelector('input[name=password]').type), 'text');
	await page.run(() => document.querySelector('.signin input[type=checkbox]').click());
	await page.click('.signin button', 'Sign in');
	await page.until(() => document.querySelector('.app'));
	await page.reload();
	await page.until(() => document.querySelector('.app') || document.querySelector('.signin form'));
	assert.equal(await page.run(() => !!document.querySelector('.app')), true, 'stay signed in');
	await page.click('.topbar button', 'Sign out');
	await page.until(() => document.querySelector('.signin form'));
	assert.equal(await page.run(() => sessionStorage.length + localStorage.length), 0);
	assert.deepEqual(page.problems, []);
});
