// A minimal Chrome DevTools Protocol client for the browser tests: it starts
// headless Chrome, which needs no display, and drives one page of it with
// Node's own WebSocket, so the tests need nothing from npm.

import { spawn, execFileSync } from 'node:child_process';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

// chromePath is the Chrome to run: $CHROME, or the first Chrome or
// Chromium on the PATH; null if there is none, and the tests skip.
export function chromePath() {
	if (process.env.CHROME) {
		return process.env.CHROME;
	}
	for (const name of ['google-chrome', 'google-chrome-stable', 'chromium', 'chromium-browser']) {
		try {
			return execFileSync('which', [name]).toString().trim();
		} catch {
			// not this one
		}
	}
	return null;
}

const sleep = ms => new Promise(r => setTimeout(r, ms));

// launch starts Chrome with an empty profile and returns its page. With
// insecure, it accepts any certificate, for a gateway on a test's own.
export async function launch({ width = 1280, height = 860, dark = false, insecure = false } = {}) {
	const profile = mkdtempSync(join(tmpdir(), 'amf-chrome-'));
	const port = 9300 + Math.floor(Math.random() * 600);
	// --password-store=basic keeps Chrome from asking the desktop's keyring
	// over D-Bus for the key its cookies are stored with, before its first
	// request: with no keyring to answer, that request waits out D-Bus's
	// 25-second timeout.
	const flags = ['--headless=new', '--no-sandbox', '--disable-gpu', '--no-first-run', '--password-store=basic', '--remote-debugging-port=' + port, '--user-data-dir=' + profile];
	if (insecure) {
		flags.push('--ignore-certificate-errors');
	}
	const chrome = spawn(chromePath(), [...flags, 'about:blank'], { stdio: 'ignore', detached: true });
	let socket;
	for (let i = 0; i < 100 && !socket; i++) {
		try {
			const page = (await (await fetch('http://127.0.0.1:' + port + '/json/list')).json()).find(t => t.type === 'page');
			if (page) {
				socket = new WebSocket(page.webSocketDebuggerUrl);
			}
		} catch {
			// not up yet
		}
		if (!socket) {
			await sleep(100);
		}
	}
	if (!socket) {
		chrome.kill();
		throw new Error('Chrome did not start');
	}
	await new Promise((resolve, reject) => {
		socket.onopen = resolve;
		socket.onerror = reject;
	});
	let id = 0;
	const replies = new Map();
	const problems = [];
	socket.onmessage = m => {
		const msg = JSON.parse(m.data);
		if (msg.id && replies.has(msg.id)) {
			const { resolve, reject } = replies.get(msg.id);
			replies.delete(msg.id);
			msg.error ? reject(new Error(msg.error.message)) : resolve(msg.result);
		} else if (msg.method === 'Runtime.exceptionThrown') {
			problems.push(msg.params.exceptionDetails.exception?.description ?? msg.params.exceptionDetails.text);
		}
	};
	const send = (method, params = {}) => new Promise((resolve, reject) => {
		replies.set(++id, { resolve, reject });
		socket.send(JSON.stringify({ id, method, params }));
	});
	await send('Runtime.enable');
	await send('Page.enable');
	await send('Emulation.setFocusEmulationEnabled', { enabled: true }); // focus events, as in a focused window
	const page = {
		problems,
		send,
		async size(w, h) {
			await send('Emulation.setDeviceMetricsOverride', { width: w, height: h, deviceScaleFactor: 1, mobile: false });
		},
		async goto(url) {
			await send('Page.navigate', { url });
			await page.until(() => document.readyState === 'complete');
		},
		async reload() {
			await send('Page.reload');
			await sleep(300);
			await page.until(() => document.readyState === 'complete');
		},
		// run runs fn in the page with args, which pass through JSON, and
		// returns its result.
		async run(fn, ...args) {
			const r = await send('Runtime.evaluate', { expression: '(' + fn + ')(...' + JSON.stringify(args) + ')', awaitPromise: true, returnByValue: true });
			if (r.exceptionDetails) {
				throw new Error(r.exceptionDetails.exception?.description ?? r.exceptionDetails.text);
			}
			return r.result.value;
		},
		// until waits for fn to return something true in the page.
		async until(fn, timeout = 8000, ...args) {
			const end = Date.now() + timeout;
			for (;;) {
				try {
					if (await page.run(fn, ...args)) {
						return;
					}
				} catch {
					// the page may be between documents
				}
				if (Date.now() > end) {
					throw new Error('timed out waiting for ' + String(fn).slice(0, 140));
				}
				await sleep(50);
			}
		},
		// click clicks the first element matching selector whose text holds
		// text, if given, once there is one.
		async click(selector, text) {
			await page.until((s, t) => [...document.querySelectorAll(s)].some(e => t == null || e.textContent.includes(t)), 8000, selector, text);
			await page.run((s, t) => [...document.querySelectorAll(s)].find(e => t == null || e.textContent.includes(t)).click(), selector, text);
		},
		// type types text into the first field matching selector as a person
		// does, an input event per character, then presses Enter if asked;
		// the field keeps the focus.
		async type(selector, text, { enter = false } = {}) {
			await page.until(s => document.querySelector(s), 8000, selector);
			await page.run((s, t, enter) => {
				const el = document.querySelector(s);
				el.focus();
				const set = Object.getOwnPropertyDescriptor(el.constructor.prototype, 'value').set;
				set.call(el, '');
				el.dispatchEvent(new Event('input', { bubbles: true }));
				for (const c of t) {
					set.call(el, el.value + c);
					el.dispatchEvent(new Event('input', { bubbles: true }));
				}
				if (enter) {
					el.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
				}
			}, selector, text, enter);
		},
		// mark gives the element fn finds in the page the ID id, for later
		// selectors.
		async mark(fn, id) {
			await page.until(fn, 8000);
			await page.run((f, id) => (0, eval)('(' + f + ')')().setAttribute('id', id), String(fn), id);
		},
		// close stops Chrome and removes its profile, which holds tens of
		// megabytes, once Chrome has exited.
		async close() {
			socket.close();
			// Chrome runs in a process group of its own (detached), which is
			// stopped entire, since its helper processes outlive it and go on
			// writing into the profile. It may have exited already, and then
			// sends no exit event.
			if (chrome.exitCode === null && chrome.signalCode === null) {
				const exited = new Promise(r => chrome.once('exit', r));
				try {
					process.kill(-chrome.pid, 'SIGTERM');
				} catch {
					chrome.kill();
				}
				await Promise.race([exited, sleep(5000)]);
			}
			try {
				process.kill(-chrome.pid, 'SIGKILL');
			} catch {
				// the group is gone
			}
			const remove = () => rmSync(profile, { recursive: true, force: true, maxRetries: 10, retryDelay: 100 });
			try {
				remove();
			} catch {
				process.once('exit', () => {
					try {
						remove();
					} catch {
						// left in the temp folder
					}
				});
			}
		},
	};
	await page.size(width, height);
	if (dark) {
		await send('Emulation.setEmulatedMedia', { features: [{ name: 'prefers-color-scheme', value: 'dark' }] });
	}
	return page;
}
