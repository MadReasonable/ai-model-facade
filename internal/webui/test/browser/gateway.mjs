// A gateway for the browser tests: built from this checkout, started on a
// free port from config files a test writes, with keys for an admin and a
// reader, and stopped when the test is done. Its one backend isn't there,
// which a gateway starts with as a warning; the UI's tests need none.

import { spawn, execFileSync } from 'node:child_process';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { createServer } from 'node:net';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';

const repo = resolve(import.meta.dirname, '../../../..');
let binary = null;

function build() {
	if (!binary) {
		const dir = mkdtempSync(join(tmpdir(), 'amf-bin-'));
		process.on('exit', () => rmSync(dir, { recursive: true, force: true }));
		binary = join(dir, 'ai-model-facade');
		execFileSync('go', ['build', '-o', binary, './cmd/ai-model-facade'], { cwd: repo, stdio: 'inherit' });
	}
	return binary;
}

function freePort() {
	return new Promise((ok, fail) => {
		const s = createServer().listen(0, '127.0.0.1', () => {
			const { port } = s.address();
			s.close(() => ok(port));
		});
		s.on('error', fail);
	});
}

export const adminKey = 'admin-key';
export const readerKey = 'reader-key';

export const config = port => `server:
  listen: 127.0.0.1:${port}
  adminApi:
    read: {allowedKeys: [viewer]}
    write: {allowedKeys: [admin]}
vars:
  noThinking: &noThinking
    chat_template_kwargs: {enable_thinking: false}
backends:
  local:
    type: llamaSwap
    baseUrl: http://127.0.0.1:1 # nothing listens here
  hosted:
    type: openWebUi
    baseUrl: http://127.0.0.1:1
models:
  swap:
    backend: local
    params: *noThinking
    candidates:
      - model: a
      - model: b
        loadIfNone: true
  single:
    backend: local
    params: *noThinking
    candidates:
      - model: c
`;

export const secrets = `server:
  apiKeys:
    - {name: admin, key: ${adminKey}}
    - {name: viewer, key: ${readerKey}}
`;

// start starts a gateway, and returns its URL, its files and a stop.
export async function start() {
	const dir = mkdtempSync(join(tmpdir(), 'amf-gw-'));
	const port = await freePort();
	const files = { config: join(dir, 'config.yaml'), secrets: join(dir, 'secrets.yaml') };
	writeFileSync(files.config, config(port));
	writeFileSync(files.secrets, secrets, { mode: 0o600 });
	const proc = spawn(build(), ['serve', files.config, '--secrets', files.secrets], { stdio: 'ignore' });
	const url = 'http://127.0.0.1:' + port;
	for (let i = 0; i < 200; i++) {
		try {
			if ((await fetch(url + '/health')).ok) {
				break;
			}
		} catch {
			// not up yet
		}
		await new Promise(r => setTimeout(r, 50));
	}
	return {
		url,
		files,
		read: name => readFileSync(files[name], 'utf8'),
		// api reads an admin endpoint as the admin.
		api: async (path, key = adminKey) => (await fetch(url + path, { headers: { Authorization: 'Bearer ' + key } })).json(),
		stop() {
			proc.kill();
			rmSync(dir, { recursive: true, force: true });
		},
	};
}

// signIn opens the UI at hash and signs in with key.
export async function signIn(page, gw, hash = '#/server', key = adminKey) {
	await page.goto(gw.url + '/amf/ui/' + hash);
	await page.until(() => document.querySelector('.signin form'));
	await page.type('input[name=password]', key);
	await page.click('.signin button', 'Sign in');
	await page.until(() => document.querySelector('.app .head-row h1'));
}
