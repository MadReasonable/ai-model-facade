// Keys and names on the Clients card: keys are hashed in the page, names
// are renamed in place, and the places a rename leaves behind are offered
// the new name.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { chromePath, launch } from './chrome.mjs';
import { adminKey, signIn, start } from './gateway.mjs';

const skip = !chromePath() && 'no Chrome on the PATH; set CHROME to one';
const sleep = ms => new Promise(r => setTimeout(r, ms));
const hash = key => 'sha256:' + createHash('sha256').update(key).digest('hex');

async function session(t, options) {
	const gw = await start();
	const page = await launch(options);
	t.after(async () => {
		gw.stop();
		await page.close();
	});
	return { gw, page };
}

const nameField = name => `() => [...document.querySelectorAll('.name-cell input')].find(i => i.value === '${name}')`;
const keyField = name => `() => [...document.querySelectorAll('.name-cell input')].find(i => i.value === '${name}').closest('tr').querySelector('.reveal input')`;

test('a new key is hashed in the page, shown until applied, and the page keeps its sign-in when its own key changes', { skip }, async t => {
	const { gw, page } = await session(t);
	await signIn(page, gw);
	await page.run(f => (0, eval)(f)().closest('tr').querySelector('button[title="Make a new key"]').click(), nameField('viewer'));
	await page.click('.dialog .btn.primary', 'Make a new key');
	await page.until(() => document.querySelector('.dialog')?.textContent.includes('Hash, as'));
	const [key, shownHash] = await page.run(() => [...document.querySelectorAll('.dialog .secret-once span')].map(s => s.textContent));
	assert.equal(shownHash, hash(key));
	assert.ok((await gw.api('/amf/config/server/apiKeys')).some(k => k.name === 'viewer' && k.hash === shownHash), 'the draft holds the hash');
	await page.click('.dialog .btn.primary', 'Done');
	await page.mark(eval(keyField('viewer')), 'viewerKey');
	assert.equal(await page.run(() => document.querySelector('#viewerKey').value.startsWith('amf-') && document.querySelector('#viewerKey').classList.contains('secret')), true, 'the key is there, as dots');

	await page.mark(eval(keyField('admin')), 'adminKey');
	await page.type('#adminKey', 'typed-key', { enter: true });
	await sleep(600);
	assert.ok((await gw.api('/amf/config/server/apiKeys')).some(k => k.name === 'admin' && k.hash === hash('typed-key')));

	await page.click('.topbar button', 'Apply');
	await page.until(() => document.querySelector('.dialog')?.textContent.includes('signs in with the new key') && document.querySelector('.dialog .diff'));
	await page.click('.dialog .btn.primary', 'Apply');
	await page.until(() => document.querySelector('.topbar')?.textContent.includes('config live'));
	assert.equal(await page.run(() => document.querySelector('.topbar .who').textContent), 'admin', 'still signed in');
	const status = async k => (await fetch(gw.url + '/amf/config-status', { headers: { Authorization: 'Bearer ' + k } })).status;
	assert.equal(await status('typed-key'), 200);
	assert.equal(await status(key), 200);
	assert.equal(await status(adminKey), 401);
	assert.deepEqual(page.problems, []);
});

test('a rename leaves the places naming the old name to the banner, which renames them', { skip }, async t => {
	const { gw, page } = await session(t);
	await signIn(page, gw);
	await page.mark(eval(nameField('viewer')), 'viewerName');
	await page.type('#viewerName', 'reader');
	await sleep(800);
	assert.equal(await page.run(() => document.body.textContent.includes('is still named in')), false, 'no banner while the name is typed');
	await page.run(() => document.activeElement.blur());
	await page.until(() => [...document.querySelectorAll('.banner')].some(b => b.textContent.includes('viewer is still named in server.adminApi.read.allowedKeys')));
	assert.deepEqual((await gw.api('/amf/config/server/apiKeys')).map(k => k.name).sort(), ['admin', 'reader'], 'only the renamed key changed');
	await page.click('.banner button', 'Rename it to');
	await page.until(() => !document.body.textContent.includes('is still named in'));
	assert.deepEqual(await gw.api('/amf/config/server/adminApi/read/allowedKeys'), ['reader']);
	assert.deepEqual(page.problems, []);
});

test('a group and a keyMap entry are renamed in place, the keyMap\'s key kept', { skip }, async t => {
	const { gw, page } = await session(t);
	await signIn(page, gw);
	await page.click('.card button', 'Add a group');
	await page.type('.dialog input', 'family');
	await page.click('.dialog .btn.primary', 'OK');
	await page.mark(eval(nameField('family')), 'groupName');
	await page.type('#groupName', 'household');
	await sleep(800);
	assert.deepEqual(Object.keys((await gw.api('/amf/config/server')).groups), ['household']);

	await page.run(() => (location.hash = '#/backend/hosted'));
	await page.until(() => document.querySelector('.head-row h1')?.textContent.startsWith('hosted'));
	await page.type('.field input[placeholder=name]', 'admin');
	await page.type('.field .reveal input[placeholder=value]', 'sk-for-admin');
	await page.click('.field .kv button[title=Add]');
	await sleep(600);
	await page.mark(() => [...document.querySelectorAll('.kv input')].find(i => i.value === 'admin'), 'mapName');
	await page.type('#mapName', 'household');
	await sleep(800);
	assert.deepEqual((await gw.api('/amf/config/backends/hosted')).keyMap, { household: '<redacted>' });
	assert.match(gw.read('secrets'), /^server:/, 'the secrets file is untouched until a save');
	assert.deepEqual(page.problems, []);
});
