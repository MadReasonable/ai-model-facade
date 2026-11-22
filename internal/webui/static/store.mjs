// The UI's state, shared by every view: the schema, the draft as GET
// /amf/config shows it, its anchors, the status of the three copies, and
// what each facade routes to now. Views read it with useStore and change
// the config through the actions here, each of which refreshes the state
// when it is done, so every view shows the draft as it now is.

import { useEffect, useState } from './vendor/hooks.mjs';
import { ApiError, configURL, entriesOf, followModels, followStatus, request } from './api.mjs';

const listeners = new Set();

export const state = {
	phase: 'signin', // signin | loading | ready
	signInError: null,
	caller: null, // {name, write}
	schema: null,
	draft: null, // the draft's JSON
	draftETag: null,
	anchors: [],
	status: null,
	facades: new Map(), // by name, from GET /amf/models
	backups: [],
	selection: { kind: 'server' },
	view: 'properties', // properties | yaml | vars
	scrollTo: null, // the id of a Card to scroll to once the sheet shows it
	unreachable: null, // {error, retry}: why the status can't be read, and the seconds to the next try; null while it can
};

function changed() {
	for (const f of listeners) {
		f();
	}
}

export function set(patch) {
	Object.assign(state, patch);
	changed();
}

// useStore re-renders the component on every change of the state.
export function useStore() {
	const [, tick] = useState(0);
	useEffect(() => {
		const f = () => tick(n => n + 1);
		listeners.add(f);
		return () => listeners.delete(f);
	}, []);
	return state;
}

// load reads everything a signed-in UI shows. It fails only if the status
// can't be read, which is how sign-in finds out whether the key may use the
// admin API.
export async function load() {
	const status = await request('GET', '/amf/config-status');
	const schema = state.schema ?? (await request('GET', '/amf/schema')).data;
	set({ caller: status.data.caller, schema });
	await refresh();
}

// refresh reads the draft, its anchors and the status again; given the
// status, as the status stream sends it, it reads the rest only.
export async function refresh(given) {
	const [draft, anchors, status, backups] = await Promise.all([
		request('GET', '/amf/config'),
		request('GET', '/amf/config-anchors'),
		given ?? request('GET', '/amf/config-status').then(r => r.data),
		request('GET', '/amf/config-backups').catch(() => null),
	]);
	set({
		draft: draft.data,
		draftETag: draft.etag,
		anchors: anchors.data.anchors,
		status,
		caller: status.caller,
		backups: backups ? backups.data.backups : state.backups,
		phase: 'ready',
	});
}

// refreshFacades reads what each facade routes to now. It reads every
// backend's state, so it runs only while the page is on screen.
export async function refreshFacades() {
	try {
		showFacades((await request('GET', '/amf/models')).data);
	} catch {
		// a key that isn't a client of the gateway sees no facades
	}
}

function showFacades(list) {
	set({ facades: new Map(list.models.map(f => [f.id, f])) });
}

// watch keeps the status and what each facade routes to current, so a
// change another admin, a reload or another client made shows here. It
// follows GET /amf/config-status?follow, and reads the draft and the rest
// again with refresh only when the status says the draft or the running
// config changed (took), and GET /amf/models?follow, each with the same
// backoff. While the status can't be read at all, unreachable says why and
// when the next try is; the models stream fails with it, since the same
// gateway serves both. It returns a function that stops both.
export function watch() {
	const stops = [
		keepFollowing({
			follow: followStatus,
			onData: data => took(JSON.parse(data)).catch(signedOutBy),
			poll: async () => took((await request('GET', '/amf/config-status')).data),
			every: 5000,
			onReach: unreachable => state.unreachable !== unreachable && set({ unreachable }),
		}),
		keepFollowing({ follow: followModels, onData: data => showFacades(JSON.parse(data)), poll: async () => showFacades((await request('GET', '/amf/models')).data), every: 15000 }),
	];
	return () => {
		stops.forEach(stop => stop());
		set({ unreachable: null });
	};
}

// keepFollowing follows a stream, giving onData each event's data. Once the
// stream ends, or can't be had, it reads with poll instead: if that works,
// it reads again every ms while the page is on screen, trying the stream
// again before each read; if it doesn't either, it tells onReach why, and
// tries the stream again after 1 s, doubling the wait each time up to
// 30 s, as the log panel does (useLogStream), until the stream or a read
// works again, which it tells onReach with null. It returns a function that
// stops it.
function keepFollowing({ follow, onData, poll, every, onReach = () => {} }) {
	let stopped = false;
	let stream = null;
	let timer = null;
	let retry = 1000;
	const reached = () => {
		retry = 1000;
		onReach(null);
	};
	const start = () => {
		stream = follow(({ event, data }) => {
			if (event === 'message') {
				reached();
				onData(data);
			}
		});
		stream.done.then(later, e => {
			signedOutBy(e);
			later();
		});
	};
	const later = async () => {
		stream = null;
		if (stopped) {
			return;
		}
		if (state.phase !== 'ready' || document.visibilityState !== 'visible') {
			timer = setTimeout(later, every);
			return;
		}
		try {
			await poll();
		} catch (e) {
			signedOutBy(e);
			if (!stopped) {
				onReach({ error: errorText(e), retry: retry / 1000 });
				timer = setTimeout(start, retry);
				retry = Math.min(retry * 2, 30000);
			}
			return;
		}
		reached();
		if (!stopped) {
			timer = setTimeout(start, every);
		}
	};
	start();
	return () => {
		stopped = true;
		clearTimeout(timer);
		stream?.stop();
	};
}

// took shows status, and, if the draft's ETag or the running config's state
// differs from what the page last read, reads the rest again with it.
async function took(status) {
	if (status.draft.etag !== state.draftETag || JSON.stringify(status.live) !== JSON.stringify(state.status?.live)) {
		return refresh(status);
	}
	set({ status, caller: status.caller });
}

// signedOutBy sends the page back to sign-in if e says the gateway no longer
// accepts its key for the admin API.
function signedOutBy(e) {
	if (e instanceof ApiError && (e.status === 401 || e.status === 403)) {
		set({ phase: 'signin', signInError: 'The gateway no longer accepts this key for the admin API: ' + e.message });
	}
}

// sharing returns, for the path of a value in the config, the anchor it
// reads, if it reads one: {anchor, via, at}, where at is the path of the
// user (the value itself or one that holds it) and exact is whether it is
// the value itself. Its value can't be edited in place without changing
// every other user, or, for a merge, without overriding it.
export function sharing(path) {
	const key = path.join('\u0000');
	let best = null;
	for (const a of state.anchors) {
		for (const u of a.users) {
			const at = u.path.join('\u0000');
			if ((key === at || key.startsWith(at + '\u0000')) && (!best || u.path.length > best.at.length)) {
				best = { anchor: a, via: u.via, at: u.path, exact: key === at };
			}
		}
	}
	return best;
}

// get returns the value at path in the draft, undefined if there is none.
export function get(path) {
	let v = state.draft;
	for (const k of path) {
		if (v == null || typeof v !== 'object') {
			return undefined;
		}
		v = v[k];
	}
	return v;
}

export function names(section) {
	const s = state.draft?.[section];
	return s && typeof s === 'object' ? entriesOf(s).map(([k]) => k) : [];
}

// The names a config value may refer to, by the x-ref of its schema.
export function refNames(ref) {
	const server = state.draft?.server ?? {};
	const keys = [...new Set((server.apiKeys ?? []).map(k => k.name).filter(Boolean))];
	const groups = ['all', 'unknown', ...Object.keys(server.groups ?? {})];
	switch (ref) {
	case 'keys':
		return keys;
	case 'groups':
		return groups;
	case 'names':
		return [...keys, ...groups];
	case 'backends':
		return names('backends');
	case 'protocols':
		return state.schema?.protocols ?? [];
	}
	return [];
}

// Changes on their way to the draft: the requests sent, and the fields
// with text not sent yet, each a function that sends it now. An action
// that reads or runs the draft, such as Diff or Apply, waits for them all
// first (settle), so it never acts on a draft missing what was typed.
const inFlight = new Set();
const unsent = new Set();

// track counts the request p as in flight until it settles.
export function track(p) {
	inFlight.add(p);
	p.then(() => inFlight.delete(p), () => inFlight.delete(p));
	return p;
}

// holdUnsent registers send as a field's text not sent yet; it returns the
// function that unregisters it.
export function holdUnsent(send) {
	unsent.add(send);
	return () => unsent.delete(send);
}

// settle sends every field's text not sent yet, then waits until no change
// is in flight, including those the sending started.
export async function settle() {
	for (const send of [...unsent]) {
		send();
	}
	while (inFlight.size > 0) {
		await Promise.allSettled([...inFlight]);
	}
}

// edit replaces the node at path in the draft with value, if its ETag is
// still etag, or, with etag null, adds it only if there is none; it returns
// the node's new ETag and the gateway's notes. A precondition failure means
// another admin changed the node, so the draft is read again before the
// error is passed on.
export function edit(path, value, etag, { yaml = false } = {}) {
	return track(sendEdit(path, value, etag, yaml));
}

async function sendEdit(path, value, etag, yaml) {
	try {
		const r = await request('PUT', configURL(path), { body: value, yaml, ifMatch: etag ?? undefined, ifNoneMatch: etag === null ? '*' : undefined });
		await refresh();
		return { etag: r.etag, notes: r.data?.notes ?? [] };
	} catch (e) {
		if (e instanceof ApiError && e.status === 412) {
			await refresh();
		}
		throw e;
	}
}

// remove deletes the node at path from the draft.
export function remove(path) {
	return track((async () => {
		await request('DELETE', configURL(path));
		await refresh();
	})());
}

// select shows a node of the tree, and keeps it in the URL as a new entry
// in the browser's history, so back and forward step through what was
// shown and a reload returns to it. With replace, it takes the place of
// the entry shown, as after a rename or a delete, whose old entry would
// show what no longer exists. With scrollTo, the sheet scrolls to that
// Card once it shows it; any other selection drops the request.
export function select(selection, view = state.view, { replace = false, scrollTo = null } = {}) {
	set({ selection, view, scrollTo });
	const parts = [selection.kind];
	if (selection.name !== undefined) {
		parts.push(encodeURIComponent(selection.name));
	}
	if (selection.index !== undefined) {
		parts.push(String(selection.index));
	}
	const hash = '#/' + parts.join('/') + (view === 'properties' ? '' : '?' + view);
	if (location.hash !== hash) {
		replace ? history.replaceState(null, '', hash) : history.pushState(null, '', hash);
	}
}

// followHistory shows what the URL names each time back or forward changes
// it, without adding an entry of its own; it returns a function that
// stops.
export function followHistory() {
	const onPop = () => set(selectionFromHash());
	addEventListener('popstate', onPop);
	return () => removeEventListener('popstate', onPop);
}

// selectionFromHash reads the selection from the URL, as select writes it.
export function selectionFromHash() {
	const [route, view = 'properties'] = location.hash.replace(/^#\//, '').split('?');
	const [kind, name, index] = route.split('/');
	const selection = { kind: ['server', 'backends', 'backend', 'models', 'model', 'candidate'].includes(kind) ? kind : 'server' };
	if (name !== undefined && selection.kind !== 'server' && selection.kind !== 'backends' && selection.kind !== 'models') {
		selection.name = decodeURIComponent(name);
	}
	if (index !== undefined && selection.kind === 'candidate') {
		selection.index = Number(index);
	}
	return { selection, view: ['properties', 'yaml', 'vars'].includes(view) ? view : 'properties' };
}

export { recordRename, renamedTo } from './renames.mjs';

// renameNode gives the key at path the name to in the draft, in its place,
// with POST /amf/config-rename, which keeps its value as the files hold it,
// a secret included, where a PUT would send it back as <redacted>.
export function renameNode(path, to) {
	return track((async () => {
		await request('POST', configURL(path).replace('/amf/config/', '/amf/config-rename/'), { query: { to } });
		await refresh();
	})());
}

// The keys this page made or was given for server.apiKeys, by the hash the
// gateway keeps of each, shown in plaintext until the draft is applied, so
// they can still be copied to their clients. They are held in memory only,
// never stored, so a reload forgets them too.
const freshKeys = new Map();

export function rememberKey(hash, key) {
	freshKeys.set(hash, key);
}

export function freshKey(hash) {
	return freshKeys.get(hash);
}

export function forgetKeys() {
	freshKeys.clear();
}

// errorText is what the UI shows for a failed request.
export function errorText(e) {
	if (e instanceof ApiError) {
		return e.message;
	}
	return String(e?.message ?? e);
}
