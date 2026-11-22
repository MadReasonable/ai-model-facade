// The admin endpoints, as the UI calls them: each request carries the key
// the user signed in with as a bearer key, and the JSON of the config is
// read and written with its keys in order.

const keyName = 'amf-admin-key';

// The key is kept for the tab, in sessionStorage, or, with "remember me",
// in localStorage, until sign-out. The page never sets a cookie, so no
// other site can make the browser send it.
export function storedKey() {
	try {
		return sessionStorage.getItem(keyName) ?? localStorage.getItem(keyName);
	} catch {
		return null;
	}
}

let key = storedKey();

export function signIn(newKey, remember) {
	key = newKey;
	try {
		sessionStorage.setItem(keyName, newKey);
		if (remember) {
			localStorage.setItem(keyName, newKey);
		}
	} catch {
		// storage off: the key lasts until the page is closed or reloaded
	}
}

export function signOut() {
	key = null;
	try {
		sessionStorage.removeItem(keyName);
		localStorage.removeItem(keyName);
	} catch {
		// nothing stored
	}
}

export function hasKey() {
	return key != null;
}

// currentKey is the key the page signed in with, null for none, and
// whether it is remembered beyond the tab.
export function currentKey() {
	let remembered = false;
	try {
		remembered = localStorage.getItem(keyName) != null;
	} catch {
		// storage off: not remembered
	}
	return { key, remembered };
}

// ApiError is a reply that isn't a success: status is its HTTP status, and
// message the gateway's error message.
export class ApiError extends Error {
	constructor(status, message, type) {
		super(message);
		this.status = status;
		this.type = type;
	}
}

function headers(extra) {
	const h = new Headers(extra);
	if (key) {
		h.set('Authorization', 'Bearer ' + key);
	}
	return h;
}

// request calls an admin endpoint. A JSON reply is read in order (see
// parseOrdered); yaml asks for, or sends, YAML instead. The result holds
// the reply's ETag and X-Gateway-Config header.
export async function request(method, path, { body, yaml = false, ifMatch, ifNoneMatch, query } = {}) {
	const h = { Accept: yaml ? 'application/yaml' : 'application/json' };
	if (body !== undefined) {
		h['Content-Type'] = yaml ? 'application/yaml' : 'application/json';
	}
	if (ifMatch) {
		h['If-Match'] = ifMatch;
	}
	if (ifNoneMatch) {
		h['If-None-Match'] = ifNoneMatch;
	}
	const url = query ? path + '?' + new URLSearchParams(query) : path;
	const resp = await fetch(url, { method, headers: headers(h), body: body === undefined ? undefined : typeof body === 'string' ? body : stringifyOrdered(body) });
	const text = await resp.text();
	if (!resp.ok) {
		let message = text.trim() || resp.statusText;
		let type;
		try {
			const e = JSON.parse(text).error;
			message = e.message ?? message;
			type = e.type;
		} catch {
			// not a JSON error body
		}
		throw new ApiError(resp.status, message, type);
	}
	const contentType = resp.headers.get('Content-Type') ?? '';
	return {
		status: resp.status,
		data: contentType.startsWith('application/json') ? parseOrdered(text) : text,
		etag: resp.headers.get('ETag'),
		config: resp.headers.get('X-Gateway-Config'),
	};
}

// configURL is the endpoint of the node at path in the config, each key
// escaped, so a key holding a slash is one segment.
export function configURL(path) {
	return '/amf/config' + path.map(k => '/' + encodeURIComponent(k)).join('');
}

// followLogs reads GET /amf/logs?follow; see follow.
export function followLogs(query, onEvent) {
	return follow('/amf/logs', query, onEvent);
}

// followStatus reads GET /amf/config-status?follow; see follow.
export function followStatus(onEvent) {
	return follow('/amf/config-status', {}, onEvent);
}

// followModels reads GET /amf/models?follow; see follow.
export function followModels(onEvent) {
	return follow('/amf/models', {}, onEvent);
}

// follow reads the server-sent events of path with ?follow, which
// EventSource can't, since it sends no Authorization header. onEvent gets
// each event as {event, data}; the returned function stops the stream. The
// promise it holds settles when the stream ends, for whatever reason.
function follow(path, query, onEvent) {
	const abort = new AbortController();
	const done = (async () => {
		const resp = await fetch(path + '?' + new URLSearchParams({ follow: '', ...query }), { headers: headers({ Accept: 'text/event-stream' }), signal: abort.signal });
		if (!resp.ok) {
			let message = resp.statusText;
			try {
				message = JSON.parse(await resp.text()).error.message;
			} catch {
				// not a JSON error body
			}
			throw new ApiError(resp.status, message);
		}
		const reader = resp.body.pipeThrough(new TextDecoderStream()).getReader();
		let buffer = '';
		for (;;) {
			const { value, done } = await reader.read();
			if (done) {
				return;
			}
			buffer += value;
			let end;
			while ((end = buffer.indexOf('\n\n')) >= 0) {
				const block = buffer.slice(0, end);
				buffer = buffer.slice(end + 2);
				let event = 'message';
				const data = [];
				for (const line of block.split('\n')) {
					if (line.startsWith('event: ')) {
						event = line.slice(7);
					} else if (line.startsWith('data: ')) {
						data.push(line.slice(6));
					}
				}
				if (data.length > 0) {
					onEvent({ event, data: data.join('\n') });
				}
			}
		}
	})();
	return { stop: () => abort.abort(), done };
}

// A JavaScript object lists integer-like keys first, whatever order they
// were added in, and the order of a config's keys matters: the keys a patch
// adds to a request are added in config order. So an object read from the
// gateway keeps its keys' order under this symbol when it has such a key,
// and keysOf, setKey, deleteKey and stringifyOrdered follow it.
const order = Symbol('order');
const integerLike = /^(0|[1-9][0-9]*)$/;

export function keysOf(obj) {
	return obj[order] ?? Object.keys(obj);
}

export function entriesOf(obj) {
	return keysOf(obj).map(k => [k, obj[k]]);
}

// setKey sets obj[k], appending k to the order if it is new.
export function setKey(obj, k, v) {
	if (!(k in obj)) {
		if (obj[order]) {
			obj[order].push(k);
		} else if (integerLike.test(k)) {
			obj[order] = [...Object.keys(obj), k];
		}
	}
	obj[k] = v;
}

export function deleteKey(obj, k) {
	if (obj[order]) {
		obj[order] = obj[order].filter(x => x !== k);
	}
	delete obj[k];
}

// withOrder returns a new object with obj's entries in the order keys gives.
export function withOrder(obj, keys) {
	const out = {};
	for (const k of keys) {
		setKey(out, k, obj[k]);
	}
	return out;
}

export function isObject(v) {
	return v !== null && typeof v === 'object' && !Array.isArray(v);
}

// clone returns a deep copy of a value read with parseOrdered, its orders
// kept.
export function clone(v) {
	if (Array.isArray(v)) {
		return v.map(clone);
	}
	if (isObject(v)) {
		const out = {};
		for (const [k, x] of entriesOf(v)) {
			setKey(out, k, clone(x));
		}
		return out;
	}
	return v;
}

export function stringifyOrdered(v) {
	if (Array.isArray(v)) {
		return '[' + v.map(stringifyOrdered).join(',') + ']';
	}
	if (isObject(v)) {
		return '{' + entriesOf(v).filter(([, x]) => x !== undefined).map(([k, x]) => JSON.stringify(k) + ':' + stringifyOrdered(x)).join(',') + '}';
	}
	return JSON.stringify(v ?? null);
}

// prettyOrdered is stringifyOrdered indented, for an editor.
export function prettyOrdered(v, indent = '') {
	const inner = indent + '  ';
	if (Array.isArray(v)) {
		if (v.length === 0) {
			return '[]';
		}
		return '[\n' + v.map(x => inner + prettyOrdered(x, inner)).join(',\n') + '\n' + indent + ']';
	}
	if (isObject(v)) {
		const entries = entriesOf(v).filter(([, x]) => x !== undefined);
		if (entries.length === 0) {
			return '{}';
		}
		return '{\n' + entries.map(([k, x]) => inner + JSON.stringify(k) + ': ' + prettyOrdered(x, inner)).join(',\n') + '\n' + indent + '}';
	}
	return JSON.stringify(v ?? null);
}

// parseOrdered is JSON.parse with each object's key order kept; see order.
export function parseOrdered(text) {
	let i = 0;
	const fail = () => {
		throw new SyntaxError('JSON: unexpected input at ' + i);
	};
	const space = () => {
		while (i < text.length && ' \t\n\r'.includes(text[i])) {
			i++;
		}
	};
	const string = () => {
		const start = i;
		i++;
		while (text[i] !== '"') {
			if (i >= text.length) {
				fail();
			}
			i += text[i] === '\\' ? 2 : 1;
		}
		i++;
		return JSON.parse(text.slice(start, i));
	};
	const value = () => {
		space();
		const c = text[i];
		if (c === '{') {
			i++;
			const obj = {};
			space();
			if (text[i] === '}') {
				i++;
				return obj;
			}
			for (;;) {
				space();
				if (text[i] !== '"') {
					fail();
				}
				const k = string();
				space();
				if (text[i++] !== ':') {
					fail();
				}
				setKey(obj, k, value());
				space();
				if (text[i] === ',') {
					i++;
				} else if (text[i] === '}') {
					i++;
					return obj;
				} else {
					fail();
				}
			}
		}
		if (c === '[') {
			i++;
			const list = [];
			space();
			if (text[i] === ']') {
				i++;
				return list;
			}
			for (;;) {
				list.push(value());
				space();
				if (text[i] === ',') {
					i++;
				} else if (text[i] === ']') {
					i++;
					return list;
				} else {
					fail();
				}
			}
		}
		if (c === '"') {
			return string();
		}
		const m = /^(?:true|false|null|-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?)/.exec(text.slice(i, i + 400));
		if (!m) {
			fail();
		}
		i += m[0].length;
		return JSON.parse(m[0]);
	};
	const result = value();
	space();
	if (i !== text.length) {
		fail();
	}
	return result;
}
