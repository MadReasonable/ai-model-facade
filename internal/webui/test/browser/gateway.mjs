// A gateway for the browser tests: built from this checkout, started on a
// free port from config files a test writes, with keys for an admin and a
// reader, and stopped when the test is done. Its llama-swap backend isn't
// there, which a gateway starts with as a warning, unless a test starts a
// fake one (fakeLlamaSwap) for what each facade routes to.

import { spawn, execFileSync } from 'node:child_process';
import { once } from 'node:events';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { createServer as createHTTPServer } from 'node:http';
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

// start starts a gateway, and returns its URL, its files, a stop, and down
// and up, which stop it and start it again on the same port and files, for
// a page that loses the gateway. With swap, the URL of a fakeLlamaSwap, the
// llama-swap backend is that fake.
export async function start({ swap } = {}) {
	const dir = mkdtempSync(join(tmpdir(), 'amf-gw-'));
	const port = await freePort();
	const files = { config: join(dir, 'config.yaml'), secrets: join(dir, 'secrets.yaml') };
	const text = config(port);
	writeFileSync(files.config, swap ? text.replace('baseUrl: http://127.0.0.1:1 # nothing listens here', 'baseUrl: ' + swap) : text);
	writeFileSync(files.secrets, secrets, { mode: 0o600 });
	const url = 'http://127.0.0.1:' + port;
	let proc = null;
	const up = async () => {
		proc = spawn(build(), ['serve', files.config, '--secrets', files.secrets], { stdio: 'ignore' });
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
	};
	await up();
	return {
		url,
		files,
		read: name => readFileSync(files[name], 'utf8'),
		// api reads an admin endpoint as the admin.
		api: async (path, key = adminKey) => (await fetch(url + path, { headers: { Authorization: 'Bearer ' + key } })).json(),
		async down() {
			const exited = once(proc, 'exit');
			proc.kill();
			await exited;
		},
		up,
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

// fakeLlamaSwap serves what the gateway's llama-swap adapter reads, for
// models, none of them running at first: /v1/models lists them, /running
// lists those in a state, and a chat for one that isn't ready makes it
// starting, waits while loads are held, then makes it ready and answers.
// It returns its URL, set(model, state) to put a model in a state ("ready",
// "starting", or "" for not running), hold(), which holds loads until the
// function it returns is called, and close.
export async function fakeLlamaSwap(models) {
	const states = new Map();
	let held = null;
	const json = (res, body) => {
		res.setHeader('Content-Type', 'application/json');
		res.end(JSON.stringify(body));
	};
	const server = createHTTPServer(async (req, res) => {
		let body = '';
		for await (const chunk of req) {
			body += chunk;
		}
		if (req.url === '/v1/models') {
			return json(res, { object: 'list', data: models.map(id => ({ id, object: 'model', meta: { llamaswap: { type: 'model' } } })) });
		}
		if (req.url === '/running') {
			return json(res, { running: [...states].map(([model, state]) => ({ model, state })) });
		}
		if (req.url === '/v1/chat/completions') {
			const model = JSON.parse(body).model;
			if (states.get(model) !== 'ready') {
				states.set(model, 'starting');
				await held;
				states.set(model, 'ready');
			}
			return json(res, { id: 'chat-1', object: 'chat.completion', model, choices: [{ index: 0, message: { role: 'assistant', content: 'hi' }, finish_reason: 'stop' }] });
		}
		res.statusCode = 404;
		res.end();
	});
	await new Promise(r => server.listen(0, '127.0.0.1', r));
	return {
		url: 'http://127.0.0.1:' + server.address().port,
		set: (model, state) => (state ? states.set(model, state) : states.delete(model)),
		hold() {
			let release;
			held = new Promise(r => (release = r));
			return () => {
				held = null;
				release();
			};
		},
		close: () => server.close(),
	};
}
