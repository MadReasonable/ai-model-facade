// The admin UI as an app: Chrome can install it, from its manifest, over
// HTTPS and under any base path, and a page loaded from one build offers a
// reload once the gateway runs another.
import { execFileSync } from 'node:child_process';
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { chromePath, launch } from './chrome.mjs';
import { signIn, start } from './gateway.mjs';

const noChrome = !chromePath() && 'no Chrome on the PATH; set CHROME to one';
const hasOpenSSL = (() => {
	try {
		execFileSync('openssl', ['version'], { stdio: 'ignore' });
		return true;
	} catch {
		return false;
	}
})();

// installable is what Chrome says of installing the page: its errors, and
// the manifest's.
async function installable(page) {
	const { installabilityErrors } = await page.send('Page.getInstallabilityErrors');
	const { errors, url } = await page.send('Page.getAppManifest');
	return { installabilityErrors, manifestErrors: errors, manifest: url };
}

test('Chrome can install the admin UI over HTTPS', { skip: noChrome || (!hasOpenSSL && 'no openssl on the PATH, to make the certificate') }, async t => {
	const gw = await start({ tls: true });
	const page = await launch({ insecure: true });
	t.after(async () => {
		gw.stop();
		await page.close();
	});
	await signIn(page, { ...gw, url: gw.httpsUrl });
	const got = await installable(page);
	assert.deepEqual(got.installabilityErrors, []);
	assert.deepEqual(got.manifestErrors, []);
	assert.equal(got.manifest, gw.httpsUrl + '/amf/ui/manifest.webmanifest');
	assert.deepEqual(page.problems, []);
});

test('Chrome can install the admin UI under another base path, as an app of that path', { skip: noChrome }, async t => {
	const gw = await start({ uiBasePath: '/admin' });
	const page = await launch();
	t.after(async () => {
		gw.stop();
		await page.close();
	});
	await signIn(page, gw); // 127.0.0.1 is a secure origin, as localhost is
	const got = await installable(page);
	assert.deepEqual(got.installabilityErrors, []);
	assert.equal(got.manifest, gw.url + '/admin/manifest.webmanifest');
	const scope = await page.run(async () => {
		const manifest = await (await fetch(document.querySelector('link[rel=manifest]').href)).json();
		return new URL(manifest.scope, document.querySelector('link[rel=manifest]').href).pathname;
	});
	assert.equal(scope, '/admin/');
});

test('a page loaded from one build offers a reload once the gateway runs another', { skip: noChrome }, async t => {
	const gw = await start({ version: 'v1.0.0-old' });
	const page = await launch();
	t.after(async () => {
		gw.stop();
		await page.close();
	});
	await signIn(page, gw);
	await page.until(() => document.querySelector('.topbar .states').textContent.includes('config live'));
	assert.ok(!(await page.run(() => document.querySelector('.topbar .states').textContent)).includes('new version'));
	await gw.down();
	await gw.up({ as: 'v1.0.1-new' });
	await page.until(() => document.querySelector('.topbar .states').textContent.includes('new version: reload'), 15000);
	assert.match(await page.run(() => [...document.querySelectorAll('.topbar .pill')].find(p => p.textContent.includes('new version')).title), /now runs v1\.0\.1-new; this page was loaded from v1\.0\.0-old/);
	await page.click('.topbar .pill', 'new version: reload');
	await page.until(() => document.querySelector('.app .head-row h1'));
	await page.until(() => document.querySelector('.topbar .states').textContent.includes('config live'));
	assert.ok(!(await page.run(() => document.querySelector('.topbar .states').textContent)).includes('new version'), 'no prompt once reloaded');
	assert.deepEqual(page.problems, []);
});
