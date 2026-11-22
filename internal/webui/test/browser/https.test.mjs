// The admin UI over HTTPS, on a port the gateway shares with plain HTTP,
// told apart by each connection's first byte: the page, its sign-in, and
// its status stream, which HTTP/2 carries there.
import { execFileSync } from 'node:child_process';
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { chromePath, launch } from './chrome.mjs';
import { adminKey, signIn, start } from './gateway.mjs';

const hasOpenSSL = (() => {
	try {
		execFileSync('openssl', ['version'], { stdio: 'ignore' });
		return true;
	} catch {
		return false;
	}
})();
const skip = (!chromePath() && 'no Chrome on the PATH; set CHROME to one') || (!hasOpenSSL && 'no openssl on the PATH, to make the certificate');

test('the admin UI works over HTTPS on the port it shares with plain HTTP', { skip }, async t => {
	const gw = await start({ tls: true });
	const page = await launch({ insecure: true });
	t.after(async () => {
		gw.stop();
		await page.close();
	});
	await signIn(page, { ...gw, url: gw.httpsUrl });
	assert.equal(await page.run(() => location.protocol), 'https:');
	assert.equal(await page.run(() => performance.getEntriesByType('navigation')[0].nextHopProtocol), 'h2', 'the page came over HTTP/2');
	await page.until(() => document.querySelector('.topbar').textContent.includes('config live'));

	const edit = await fetch(gw.url + '/amf/config/models/swap/description', { method: 'PUT', headers: { Authorization: 'Bearer ' + adminKey, 'Content-Type': 'application/json' }, body: '"Over plain HTTP"' });
	assert.equal(edit.status, 201, 'an edit over plain HTTP, on the same port');
	await page.until(() => document.querySelector('.topbar').textContent.includes('changes staged'), 2000);
	assert.deepEqual(page.problems, []);
});
