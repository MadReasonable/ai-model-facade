// The views of the main pane: a sheet for the server, a list for each
// section, and a sheet for each backend, facade and candidate. Each edits
// the draft through useEntry, so a change is in the draft at once and runs
// only once the draft is applied.

import { useRef, useState } from './vendor/hooks.mjs';
import { clone, configURL, entriesOf, isObject, keysOf, request, setKey, withOrder } from './api.mjs';
import { Chips, EntryBanners, SharedNote, Sheet, TextInput, useEntry, waiting, withKey } from './fields.mjs';
import { edit, errorText, freshKey, get, recordRename, refresh, rememberKey, remove, renamedTo, renameNode, select, settle, sharing, state, useStore } from './store.mjs';
import { Banner, confirm, Copy, Dialog, focusOnMount, html, Icon, openDialog, pathText, plural, Reveal, toast } from './ui.mjs';
import { keyHash } from './sha256.mjs';

// ViewSwitch picks what the main pane shows of the selection: its
// properties, its YAML, or the anchors under vars, which it may use.
export function ViewSwitch() {
	const s = useStore();
	const views = [['properties', 'Properties'], ['yaml', 'YAML']];
	return html`
		<div class="seg">
			${views.map(([v, label]) => html`<button class=${s.view === v ? 'on' : ''} onClick=${() => select(s.selection, v)}>${label}</button>`)}
			<button class=${'vars' + (s.view === 'vars' ? ' on' : '')} title="The anchors under vars, which entries share" onClick=${() => select(s.selection, 'vars')}>&vars</button>
		</div>
	`;
}

// Head is the top of a sheet: where it is, what it is, and its tools, in
// one row, with the description below it at full width, so the tools
// never narrow it. The two are the sheet's own children, so on a narrow
// screen the row can stick to the top of the sheet's pane as it scrolls
// (useScrollAway in app.mjs).
export function Head({ crumbs = [], title, subtitle, tools }) {
	return html`
		<div class="head-row">
			<div class="titles">
				${crumbs.length > 0 && html`<div class="crumbs">${crumbs.map((c, i) => html`${i > 0 && ' › '}<button onClick=${() => select(c.selection)}>${c.label}</button>`)}</div>`}
				<h1>${title}</h1>
			</div>
			<div class="tools">${tools}<${ViewSwitch} /></div>
		</div>
		<p class="sheet-description">${subtitle}</p>
	`;
}

function Card({ title, intro, tools, children }) {
	return html`
		<section class="card">
			<h2>${title}${tools && html`<span class="tools">${tools}</span>`}</h2>
			${intro && html`<p class="intro">${intro}</p>`}
			${children}
		</section>
	`;
}

function readOnly() {
	return !state.caller?.write;
}

// The server sheet

export function ServerSheet() {
	const s = useStore();
	const server = useEntry(['server']);
	const logging = useEntry(['logging']);
	const api = useEntry(['api']);
	const wait = waiting(server, logging, api);
	if (wait) {
		return wait;
	}
	const config = s.schema.config.properties;
	const serverSchema = config.server;
	const admin = serverSchema.properties.adminApi;
	const ro = readOnly();
	const sv = server.value ?? {};
	const setAdmin = x => server.commit(withKey(sv, 'adminApi', x));
	return html`
		<div class="sheet">
			<${Head} title="Server" subtitle="How the gateway listens, who may use it, and how it logs and keeps its config." />
			<${EntryBanners} entry=${server} />
			<${EntryBanners} entry=${logging} />
			<${EntryBanners} entry=${api} />
			<${Card} title="General">
				<${Sheet} schema=${serverSchema} only=${['listen', 'unknownKeys', 'targetHeader']} value=${sv} path=${['server']} readOnly=${ro} onChange=${server.commit} />
			<//>
			<${Clients} server=${server} />
			<${Card} title="Admin API" intro="Who may read and change the config over /amf, including this page, and where those endpoints listen.">
				<${Sheet} schema=${admin} only=${['read', 'write', 'listen']} value=${sv.adminApi} path=${['server', 'adminApi']} readOnly=${ro} onChange=${setAdmin} />
			<//>
			<${Card} title="Logging">
				<${Sheet} schema=${config.logging} value=${logging.value} path=${['logging']} readOnly=${ro} onChange=${logging.commit} />
				<p class="intro" style="padding-top:0">The gateway keeps its own copy of its recent log for the log panel and GET /amf/logs.</p>
				<${Sheet} schema=${admin.properties.logs} value=${sv.adminApi?.logs} path=${['server', 'adminApi', 'logs']} readOnly=${ro} onChange=${x => setAdmin(withKey(sv.adminApi, 'logs', x))} />
			<//>
			<${Backups} server=${server} setAdmin=${setAdmin} />
			<${Card} title="Client APIs" intro=${config.api.description}>
				<${Sheet} schema=${config.api} value=${api.value} path=${['api']} readOnly=${ro} onChange=${api.commit} />
			<//>
			<${Card} title="Responses conversations" intro=${serverSchema.properties.responses.description}>
				<${Sheet} schema=${serverSchema.properties.responses} value=${sv.responses} path=${['server', 'responses']} readOnly=${ro} onChange=${x => server.commit(withKey(sv, 'responses', x))} />
			<//>
		</div>
	`;
}

// references lists every place the draft names a key or a group: the
// admin lists, the groups' members, the access lists of backends and
// facades, and backends' keyMaps, whose names may be either.
function references() {
	const out = [];
	const d = state.draft ?? {};
	const list = (path, kind, names) => {
		for (const name of Array.isArray(names) ? names : []) {
			out.push({ name, kind, path, how: 'list' });
		}
	};
	const access = (path, a) => {
		list([...path, 'allowedKeys'], 'key', a?.allowedKeys);
		list([...path, 'allowedGroups'], 'group', a?.allowedGroups);
	};
	access(['server', 'adminApi', 'read'], d.server?.adminApi?.read);
	access(['server', 'adminApi', 'write'], d.server?.adminApi?.write);
	for (const [g, members] of entriesOf(isObject(d.server?.groups) ? d.server.groups : {})) {
		list(['server', 'groups', g], 'key', members);
	}
	for (const section of ['backends', 'models']) {
		for (const [entry, e] of entriesOf(isObject(d[section]) ? d[section] : {})) {
			access([section, entry], e);
			if (section === 'backends' && isObject(e?.keyMap)) {
				for (const name of keysOf(e.keyMap)) {
					out.push({ name, kind: 'either', path: [section, entry, 'keyMap'], how: 'keyMap' });
				}
			}
		}
	}
	return out;
}

// Unnamed is the banner at the bottom of the Clients card, at the bottom
// so it moves nothing above it as it comes and goes: every name the draft
// uses that no key or group has, where it is used, and, for a name this
// page renamed, a button that gives every place the new name. The gateway
// refuses to apply a draft that names what doesn't exist, so it shows what
// is left to do; a draft may be inconsistent in between, as a rename across
// it is.
function Unnamed({ hold, readOnly: ro }) {
	const s = useStore();
	const [busy, setBusy] = useState(false);
	if (hold) {
		return null;
	}
	const server = s.draft?.server ?? {};
	const keyNames = new Set((server.apiKeys ?? []).map(k => k.name));
	const groupNames = new Set(['all', 'unknown', ...keysOf(isObject(server.groups) ? server.groups : {})]);
	const defined = r => (r.kind === 'key' ? keyNames.has(r.name) : r.kind === 'group' ? groupNames.has(r.name) : keyNames.has(r.name) || groupNames.has(r.name));
	const byName = new Map();
	for (const r of references().filter(r => !defined(r))) {
		byName.set(r.name, [...(byName.get(r.name) ?? []), r]);
	}
	if (byName.size === 0) {
		return null;
	}
	const follow = async (name, to) => {
		setBusy(true);
		const failed = [];
		for (const r of byName.get(name)) {
			try {
				if (r.how === 'keyMap') {
					await renameNode([...r.path, name], to);
				} else {
					const now = get(r.path) ?? [];
					await edit(r.path, [...new Set(now.map(x => (x === name ? to : x)))], undefined);
				}
			} catch (e) {
				failed.push(pathText(r.path) + ': ' + errorText(e));
			}
		}
		setBusy(false);
		if (failed.length > 0) {
			toast(failed.join('\n'));
		}
	};
	const open = path => select(path[0] === 'backends' ? { kind: 'backend', name: path[1] } : path[0] === 'models' ? { kind: 'model', name: path[1] } : { kind: 'server' }, 'properties');
	return html`
		<div class="fields" style="border-top:1px solid var(--border);padding-top:12px">
			${[...byName].map(([name, places]) => {
				const to = renamedTo(places[0].kind === 'group' ? 'group' : 'key', name) ?? renamedTo('group', name);
				const target = to && (keyNames.has(to) || groupNames.has(to)) ? to : null;
				return html`
					<${Banner} kind="warn">
						<div><code>${name}</code> is still named in ${places.map((p, i) => html`${i > 0 && ', '}<button class="link" onClick=${() => open(p.path)}><code>${pathText(p.path)}</code></button>`)}, but no key or group has that name, so the draft can't be applied until it does.</div>
						${target && !ro && html`<div style="margin-top:8px"><button class="btn small" disabled=${busy} onClick=${() => follow(name, target)}>Rename ${places.length === 1 ? 'it' : 'them'} to <code>${target}</code></button></div>`}
					<//>
				`;
			})}
		</div>
	`;
}

// newKey makes a key in the form hash-key prints: amf- and 32 random bytes
// in unpadded base64url.
function newKey() {
	const b = crypto.getRandomValues(new Uint8Array(32));
	return 'amf-' + btoa(String.fromCharCode(...b)).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

// Clients edits server.apiKeys and server.groups. A key added here is sent
// to the gateway once, which holds only its hash, so it is shown once, to
// be copied to its client.
function Clients({ server }) {
	const sv = server.value ?? {};
	const keys = sv.apiKeys ?? [];
	const groups = isObject(sv.groups) ? sv.groups : {};
	const ro = readOnly();
	const groupsOf = name => entriesOf(groups).filter(([, members]) => (members ?? []).includes(name)).map(([g]) => g);
	// A key is hashed here, as the gateway hashes it, and only its hash is
	// sent, so the key never leaves the browser. The page remembers the key
	// by its hash, to show it until the draft is applied.
	const setKeyAt = async (i, name, key, made) => {
		const entry = { name, hash: keyHash(key) };
		if (i < keys.length && keys[i].hash) {
			rowIDs.current.byHash.set(entry.hash, rowID(keys[i])); // the row stays the same row
		}
		const list = i < keys.length ? keys.map((k, j) => (j === i ? entry : k)) : [...keys, entry];
		rememberKey(entry.hash, key);
		if (!(await server.commit(withKey(sv, 'apiKeys', list))) || !made) {
			return;
		}
		openDialog(close => html`
			<${Dialog} title=${'New key for ' + name} foot=${html`<button class="btn primary" onClick=${() => close()}>Done</button>`}>
				<p>Give the key to the client. The gateway keeps only its hash, so this page shows the key until the draft is applied, and never again after that.</p>
				<label class="stack">Key<div class="secret-once"><span style="flex:1">${key}</span><${Copy} text=${key} /></div></label>
				<label class="stack">Hash, as server.apiKeys holds it<div class="secret-once plain"><span style="flex:1">${entry.hash}</span><${Copy} text=${entry.hash} /></div></label>
			<//>
		`);
	};
	const addKey = async () => {
		const added = await openDialog(close => html`<${AddKeyDialog} names=${[...new Set(keys.map(k => k.name))]} close=${close} />`);
		if (added) {
			setKeyAt(keys.length, added.name, added.key, added.generated);
		}
	};
	const newKeyAt = async i => {
		const yes = await confirm({ title: 'Make a new key for ' + keys[i].name, body: 'The new key replaces this one once the draft is applied, and a client still sending the old key is refused from then on.', yes: 'Make a new key' });
		if (yes) {
			setKeyAt(i, keys[i].name, newKey(), true);
		}
	};
	// A rename refused or cancelled shows the name as it is again; the
	// field keeps what was typed otherwise, since the name it is given
	// doesn't change.
	const [nameResets, setNameResets] = useState(0);
	// Each row is keyed by its entry, not its place: an entry changed moves
	// in the list, as the gateway lists the main config's keys before the
	// secrets file's, and a field being typed in has to move with it. An
	// entry is known by its hash, which a rename keeps and a new key hands
	// on to the next (setKeyAt).
	const rowIDs = useRef({ next: 0, byHash: new Map() });
	const rowID = k => {
		const ids = rowIDs.current;
		if (!k.hash) {
			return 'unhashed:' + k.name;
		}
		if (!ids.byHash.has(k.hash)) {
			ids.byHash.set(k.hash, ids.next++);
		}
		return ids.byHash.get(k.hash);
	};
	// The banner of names left behind waits while a name is being typed,
	// so it doesn't show a name a letter at a time.
	const [editingNames, setEditingNames] = useState(false);
	// Lowercase, so Preact listens for focusin and focusout themselves: an
	// element has no onfocusin property for it to find the name by.
	const nameFocus = {
		onfocusin: e => setEditingNames(!!e.target.closest('.name-cell')),
		onfocusout: () => setEditingNames(false),
	};
	const renameAt = (i, name) => {
		const n = (name ?? '').trim();
		const old = keys[i].name;
		const keep = () => setNameResets(r => r + 1);
		if (!n || n === old) {
			keep();
			return;
		}
		if (['all', 'unknown'].includes(n) || isObject(groups) && n in groups) {
			toast(n + ' is the name of a group, so it can\'t be a key\'s too.');
			keep();
			return;
		}
		// Only a name no other key keeps is renamed; the places that named
		// it are offered the new one (Unnamed).
		if (!keys.some((k, j) => j !== i && k.name === old)) {
			recordRename('key', old, n);
		}
		server.commit(withKey(sv, 'apiKeys', keys.map((k, j) => (j === i ? withKey(k, 'name', n) : k))));
	};
	const renameGroup = async (old, name) => {
		const n = (name ?? '').trim();
		if (!n || n === old) {
			setNameResets(r => r + 1);
			return;
		}
		if (['all', 'unknown'].includes(n) || keys.some(k => k.name === n) || n in groups) {
			toast(n + ' is already the name of a key or a group.');
			setNameResets(r => r + 1);
			return;
		}
		try {
			await renameNode(['server', 'groups', old], n);
			recordRename('group', old, n);
		} catch (e) {
			toast(errorText(e));
			setNameResets(r => r + 1);
		}
	};
	const removeKey = async i => {
		const k = keys[i];
		const yes = await confirm({ title: 'Remove a key', body: 'The client named ' + k.name + ' can no longer use this key once the draft is applied.' + (keys.filter(x => x.name === k.name).length > 1 ? ' Its other keys still work.' : ''), yes: 'Remove', danger: true });
		if (yes) {
			server.commit(withKey(sv, 'apiKeys', keys.filter((_, j) => j !== i)));
		}
	};
	const setGroups = g => server.commit(withKey(sv, 'groups', keysOf(g).length === 0 ? undefined : g));
	const addGroup = async () => {
		const name = await openDialog(close => html`<${NameDialog} title="Add a group" label="Group name" taken=${[...keysOf(groups), 'all', 'unknown', ...keys.map(k => k.name)]} close=${close} />`);
		if (name) {
			const g = clone(groups);
			setKey(g, name, []);
			setGroups(g);
		}
	};
	const keyNames = [...new Set(keys.map(k => k.name).filter(Boolean))];
	return html`
		<${Card} title="Clients" intro="The keys clients send, each with a name that the log and the access lists use, and the groups of names that access lists can allow." tools=${!ro && html`<button class="btn small" onClick=${addKey}><${Icon} name="key" size=${14} />Add a key</button>`}>
			<div ...${nameFocus}>
			<table class="list">
				<thead><tr><th>Name</th><th>Key</th><th>Groups</th><th></th></tr></thead>
				<tbody>
					${keys.length === 0 && html`<tr><td colspan="4" class="empty">No keys, so the gateway accepts every request. Access lists can then only name the built-in group unknown.</td></tr>`}
					${keys.map((k, i) => {
						const fresh = k.hash && freshKey(k.hash);
						return html`
							<tr key=${rowID(k)}>
								<td class="name-cell"><${TextInput} key=${'name:' + nameResets} value=${k.name} readOnly=${ro} onCommit=${n => renameAt(i, n)} /></td>
								<td><${TextInput} value=${fresh ?? ''} placeholder="••••••••••••" secret live readOnly=${ro} onCommit=${key => key && setKeyAt(i, k.name, key, false)} /></td>
								<td>${groupsOf(k.name).map(g => html`<span class="badge" style="margin-right:4px">${g}</span>`)}</td>
								<td class="order">${!ro && html`
									<button class="btn ghost icon" title="Make a new key" onClick=${() => newKeyAt(i)}><${Icon} name="refresh" /></button>
									<button class="btn ghost icon" title="Remove this key" onClick=${() => removeKey(i)}><${Icon} name="trash" /></button>
								`}</td>
							</tr>
						`;
					})}
				</tbody>
			</table>
			<div class="fields" style="border-top:1px solid var(--border)">
				${entriesOf(groups).map(([name, members], gi) => html`
					<div class="field" key=${gi}>
						<div class="name name-cell"><${TextInput} key=${'group:' + nameResets} value=${name} readOnly=${ro} onCommit=${n => renameGroup(name, n)} /></div>
						<div class="control"><div class="row">
							<${Chips} values=${members ?? []} known=${keyNames} readOnly=${ro} onChange=${m => setGroups(withKey(groups, name, m))} />
							${!ro && html`<button class="btn ghost icon" title="Remove this group" onClick=${() => setGroups(withKey(groups, name, undefined))}><${Icon} name="trash" /></button>`}
						</div></div>
					</div>
				`)}
				<div class="row" style="padding:10px 0;gap:8px">
					<span class="muted" style="flex:1">${keysOf(groups).length === 0 ? 'No groups beyond the built-in all, every listed key, and unknown, any other caller.' : 'Besides these, all holds every listed key and unknown any other caller.'}</span>
					${!ro && html`<button class="btn small" onClick=${addGroup}><${Icon} name="plus" size=${14} />Add a group</button>`}
				</div>
			</div>
			</div>
			<${Unnamed} hold=${editingNames} readOnly=${ro} />
		<//>
	`;
}

function AddKeyDialog({ names, close }) {
	const [name, setName] = useState('');
	const [mode, setMode] = useState('generate');
	const [pasted, setPasted] = useState('');
	const ok = name.trim() !== '' && !['all', 'unknown'].includes(name.trim()) && (mode === 'generate' || pasted !== '');
	const submit = () => ok && close({ name: name.trim(), key: mode === 'generate' ? newKey() : pasted, generated: mode === 'generate' });
	return html`
		<${Dialog} title="Add a key" foot=${html`
			<button class="btn" onClick=${() => close()}>Cancel</button>
			<button class="btn primary" disabled=${!ok} onClick=${submit}>Add</button>
		`}>
			<label class="stack">Client name
				<input type="text" list="key-names" value=${name} ref=${focusOnMount} onInput=${e => setName(e.target.value)} onKeyDown=${e => e.key === 'Enter' && submit()} />
				<datalist id="key-names">${names.map(n => html`<option value=${n} />`)}</datalist>
			</label>
			<p>${names.includes(name.trim()) ? 'This client already has a key; it keeps it, and gets this one too, as when clients change over to a new key.' : 'All and unknown are the built-in groups, so they can\'t be names.'}</p>
			<div class="seg" style="align-self:flex-start">
				<button class=${mode === 'generate' ? 'on' : ''} onClick=${() => setMode('generate')}>Make a new key</button>
				<button class=${mode === 'paste' ? 'on' : ''} onClick=${() => setMode('paste')}>Use an existing key</button>
			</div>
			${mode === 'paste' && html`<label class="stack">Key<${Reveal} field=${hidden => html`<input type="text" class=${hidden ? 'secret' : undefined} autocomplete="off" spellcheck=${false} value=${pasted} onInput=${e => setPasted(e.target.value)} />`} /></label>`}
			<p>This page hashes the key and sends the gateway only the hash, so the key itself never leaves the browser.</p>
		<//>
	`;
}

// NameDialog asks for a name not among taken.
function NameDialog({ title, label, value = '', taken, close, children }) {
	const [name, setName] = useState(value);
	const n = name.trim();
	const clash = n !== value && taken.includes(n);
	const submit = () => n && !clash && n !== value && close(n);
	return html`
		<${Dialog} title=${title} foot=${html`
			<button class="btn" onClick=${() => close()}>Cancel</button>
			<button class="btn primary" disabled=${!n || clash || n === value} onClick=${submit}>OK</button>
		`}>
			<label class="stack">${label}<input type="text" value=${name} ref=${focusOnMount} onInput=${e => setName(e.target.value)} onKeyDown=${e => e.key === 'Enter' && submit()} /></label>
			${clash && html`<p style="color:var(--bad)">${n} is already taken.</p>`}
			${children}
		<//>
	`;
}

// Backups shows the files a save writes, whether it can, and the backups
// it made, each of which can be loaded into the draft.
function Backups({ server, setAdmin }) {
	const s = useStore();
	const ro = readOnly();
	const status = s.status;
	const load = async backup => {
		if (s.status.draft.edited && !(await confirm({ title: 'Replace the draft', body: 'The draft holds edits that aren\'t applied. Loading replaces them.', yes: 'Load', danger: true }))) {
			return;
		}
		try {
			const r = await request('POST', '/amf/config-load', { query: backup ? { backup } : {} });
			await refresh();
			toast((backup ? 'Loaded backup ' + r.data.backup : 'Loaded the files on disk') + ' into the draft.' + (r.data.notes.length ? '\n' + r.data.notes.join('\n') : ''));
		} catch (e) {
			toast(errorText(e));
		}
	};
	return html`
		<${Card} title="Files and backups" intro="A save writes the running config to these files, after copying both into a backup folder. A backup loaded into the draft runs once the draft is applied." tools=${!ro && html`<button class="btn small" onClick=${() => load()}>Load the files on disk</button>`}>
			<table class="list">
				<thead><tr><th>File</th><th>Path</th><th>State</th></tr></thead>
				<tbody>
					${status.files.map(f => html`
						<tr>
							<td>${f.role}</td>
							<td class="mono">${f.path}</td>
							<td>
								${f.changedOnDisk && html`<span class="pill warn" style="margin-right:6px">changed on disk</span>`}
								${f.saveable ? html`<span class="pill good">saveable</span>` : html`<span class="pill bad" title=${f.reason}>can't be saved</span> <span class="muted">${f.reason}</span>`}
							</td>
						</tr>
					`)}
					${status.backups && html`
						<tr>
							<td>backups</td>
							<td class="mono">${status.backups.path}</td>
							<td>${status.backups.writable ? html`<span class="pill good">writable</span>` : html`<span class="pill bad">not writable</span> <span class="muted">${status.backups.reason}</span>`}</td>
						</tr>
					`}
				</tbody>
			</table>
			<div class="fields" style="border-top:1px solid var(--border)">
				<${Sheet} schema=${s.schema.config.properties.server.properties.adminApi} only=${['configBackups']} value=${server.value?.adminApi} path=${['server', 'adminApi']} readOnly=${ro} onChange=${setAdmin} />
			</div>
			<table class="list">
				<thead><tr><th>Backup</th><th>Made</th><th>Files</th><th></th></tr></thead>
				<tbody>
					${s.backups.length === 0 && html`<tr><td colspan="4" class="empty">No backups yet. Each save makes one.</td></tr>`}
					${s.backups.map((b, i) => html`
						<tr>
							<td class="mono">${b.id}${i === 0 && html` <span class="badge">-1</span>`}</td>
							<td>${new Date(b.time).toLocaleString()}</td>
							<td>${b.files.join(', ')}</td>
							<td class="order">${!ro && html`<button class="btn small" onClick=${() => load(b.id)}>Load into the draft</button>`}</td>
						</tr>
					`)}
				</tbody>
			</table>
		<//>
	`;
}

// Reordering a list by dragging its rows, or by buttons for the keyboard.

function useReorder(count, onMove) {
	const [drag, setDrag] = useState(null);
	const [over, setOver] = useState(null);
	const rowProps = i => ({
		draggable: !readOnly(),
		onDragStart: e => {
			setDrag(i);
			e.dataTransfer.effectAllowed = 'move';
			e.dataTransfer.setData('text/plain', String(i));
		},
		onDragOver: e => {
			if (drag === null) {
				return;
			}
			e.preventDefault();
			const r = e.currentTarget.getBoundingClientRect();
			setOver({ i, after: e.clientY > r.top + r.height / 2 });
		},
		onDragEnd: () => {
			setDrag(null);
			setOver(null);
		},
		onDrop: e => {
			e.preventDefault();
			if (drag !== null && over) {
				let to = over.after ? over.i + 1 : over.i;
				if (to > drag) {
					to--;
				}
				if (to !== drag) {
					onMove(drag, to);
				}
			}
			setDrag(null);
			setOver(null);
		},
		class: 'row' + (drag === i ? ' dragging' : '') + (over && over.i === i && drag !== i ? (over.after ? ' drop-after' : ' drop-before') : ''),
	});
	const buttons = i => !readOnly() && html`
		<button class="btn ghost icon" title="Move up" disabled=${i === 0} onClick=${e => (e.stopPropagation(), onMove(i, i - 1))}><${Icon} name="up" /></button>
		<button class="btn ghost icon" title="Move down" disabled=${i === count - 1} onClick=${e => (e.stopPropagation(), onMove(i, i + 1))}><${Icon} name="down" /></button>
	`;
	return { rowProps, buttons };
}

function moved(list, from, to) {
	const out = [...list];
	const [x] = out.splice(from, 1);
	out.splice(to, 0, x);
	return out;
}

// rename gives the entry name of section the name to, in its place,
// keeping it as the files hold it.
async function rename(section, name, to) {
	await request('POST', configURL([section, name]).replace('/amf/config/', '/amf/config-rename/'), { query: { to } });
	await refresh();
}

async function renameEntry(section, name, kind) {
	const to = await openDialog(close => html`<${NameDialog} title=${'Rename ' + name} label="New name" value=${name} taken=${keysOf(state.draft[section] ?? {})} close=${close}>
		${section === 'backends' ? html`<p>Facades and candidates that name this backend aren't changed, and need the new name before the draft can be applied.</p>` : html`<p>Clients ask for a facade by its name, so they need the new name once the draft is applied.</p>`}
	<//>`);
	if (!to) {
		return;
	}
	try {
		await rename(section, name, to);
		select({ kind, name: to }, state.view, { replace: true });
	} catch (e) {
		toast(errorText(e));
	}
}

async function deleteEntry(section, name, warning) {
	const yes = await confirm({ title: 'Delete ' + name, body: html`<p>The entry leaves the draft, and stops running once the draft is applied.</p>${warning && html`<p>${warning}</p>`}`, yes: 'Delete', danger: true });
	if (!yes) {
		return false;
	}
	try {
		await remove([section, name]);
		return true;
	} catch (e) {
		toast(errorText(e));
		return false;
	}
}

// usersOf lists the facades and candidates that name backend name.
function usersOf(name) {
	const out = [];
	for (const [facade, m] of entriesOf(state.draft.models ?? {})) {
		if (!isObject(m)) {
			continue;
		}
		(m.candidates ?? []).forEach((c, i) => {
			if ((c.backend ?? m.backend) === name) {
				out.push({ facade, index: i, model: c.model });
			}
		});
	}
	return out;
}

// The section lists

export function SectionList({ section }) {
	const s = useStore();
	const entry = useEntry([section]);
	const ro = readOnly();
	const value = isObject(entry.value) ? entry.value : {};
	const rows = entriesOf(value);
	const backends = section === 'backends';
	const kind = backends ? 'backend' : 'model';
	const { rowProps, buttons } = useReorder(rows.length, (from, to) => entry.commit(withOrder(value, moved(keysOf(value), from, to))));
	const wait = waiting(entry);
	if (wait) {
		return wait;
	}
	const add = async () => {
		const made = await openDialog(close => html`<${backends ? AddBackendDialog : AddModelDialog} taken=${keysOf(value)} close=${close} />`);
		if (!made) {
			return;
		}
		try {
			if (entry.missing) {
				await edit([section], { [made.name]: made.value }, null);
			} else {
				await edit([section, made.name], made.value, null);
			}
			select({ kind, name: made.name });
		} catch (e) {
			toast(errorText(e));
		}
	};
	return html`
		<div class="sheet">
			<${Head} title=${backends ? 'Backends' : 'Models'} subtitle=${s.schema.config.properties[section].description} tools=${!ro && html`<button class="btn primary" onClick=${add}><${Icon} name="plus" />Add</button>`} />
			<${EntryBanners} entry=${entry} />
			<section class="card">
				<table class="list">
					<thead><tr>${!ro && html`<th></th>`}<th>Name</th>${backends ? html`<th>Type</th><th>Address</th><th>Candidates</th>` : html`<th>Backend</th><th>Description</th><th>Now</th>`}<th></th></tr></thead>
					<tbody>
						${rows.length === 0 && html`<tr><td colspan="6" class="empty">${backends ? 'No backends yet.' : 'No facades yet.'}</td></tr>`}
						${rows.map(([name, e], i) => html`
							<tr key=${name} ...${rowProps(i)} onClick=${() => select({ kind, name })}>
								${!ro && html`<td class="grip" title="Drag to reorder"><${Icon} name="grip" /></td>`}
								<td class="mono">${name}</td>
								${backends ? html`
									<td><span class="badge">${e?.type ?? '?'}</span></td>
									<td class="mono muted">${e?.baseUrl}</td>
									<td>${usersOf(name).length}</td>
								` : html`
									<td class="mono">${e?.backend ?? html`<span class="muted">per candidate</span>`}</td>
									<td>${e?.description}</td>
									<td><${Now} facade=${s.facades.get(name)} /></td>
								`}
								<td class="order">${buttons(i)}</td>
							</tr>
						`)}
					</tbody>
				</table>
			</section>
			${!backends && html`<p class="muted">The order of facades is the order of the model lists clients read.</p>`}
		</div>
	`;
}

// Now says where a request for a facade would go now.
function Now({ facade }) {
	if (!facade?.current) {
		return html`<span class="muted">${facade ? 'nothing eligible' : '-'}</span>`;
	}
	return html`<span class="state loaded mono">${facade.current.model}</span>`;
}

function AddBackendDialog({ taken, close }) {
	const s = useStore();
	const types = Object.keys(s.schema.backendTypes);
	const [name, setName] = useState('');
	const [type, setType] = useState(types[0]);
	const [baseUrl, setBaseUrl] = useState('');
	const n = name.trim();
	const ok = n && !taken.includes(n) && baseUrl.trim();
	const submit = () => ok && close({ name: n, value: { type, baseUrl: baseUrl.trim() } });
	return html`
		<${Dialog} title="Add a backend" foot=${html`<button class="btn" onClick=${() => close()}>Cancel</button><button class="btn primary" disabled=${!ok} onClick=${submit}>Add</button>`}>
			<label class="stack">Name<input type="text" value=${name} ref=${focusOnMount} onInput=${e => setName(e.target.value)} /></label>
			${taken.includes(n) && html`<p style="color:var(--bad)">${n} is already taken.</p>`}
			<label class="stack">Type<select value=${type} onChange=${e => setType(e.target.value)}>${types.map(t => html`<option value=${t}>${t}</option>`)}</select></label>
			<label class="stack">Address<input type="text" placeholder="http://host:port" value=${baseUrl} onInput=${e => setBaseUrl(e.target.value)} onKeyDown=${e => e.key === 'Enter' && submit()} /></label>
		<//>
	`;
}

function AddModelDialog({ taken, close }) {
	const s = useStore();
	const backends = keysOf(s.draft.backends ?? {});
	const [name, setName] = useState('');
	const [backend, setBackend] = useState(backends[0] ?? '');
	const [model, setModel] = useState('');
	const n = name.trim();
	const ok = n && !taken.includes(n) && backend && model.trim();
	const submit = () => ok && close({ name: n, value: { backend, candidates: [{ model: model.trim() }] } });
	return html`
		<${Dialog} title="Add a facade" foot=${html`<button class="btn" onClick=${() => close()}>Cancel</button><button class="btn primary" disabled=${!ok} onClick=${submit}>Add</button>`}>
			<label class="stack">Name, which clients ask for<input type="text" value=${name} ref=${focusOnMount} onInput=${e => setName(e.target.value)} /></label>
			${taken.includes(n) && html`<p style="color:var(--bad)">${n} is already taken.</p>`}
			<label class="stack">Backend<select value=${backend} onChange=${e => setBackend(e.target.value)}>${backends.map(b => html`<option value=${b}>${b}</option>`)}</select></label>
			${backends.length === 0 && html`<p>Add a backend first.</p>`}
			<label class="stack">First candidate: a model ID, alias or pattern<input type="text" value=${model} onInput=${e => setModel(e.target.value)} onKeyDown=${e => e.key === 'Enter' && submit()} /></label>
		<//>
	`;
}

// The backend sheet

export function BackendSheet({ name }) {
	const s = useStore();
	const path = ['backends', name];
	const entry = useEntry(path);
	const ro = readOnly();
	const [test, setTest] = useState(null);
	const wait = waiting(entry);
	if (wait) {
		return wait;
	}
	if (entry.missing) {
		return html`<div class="sheet"><${Banner} kind="warn">The draft has no backend named ${name}.<//></div>`;
	}
	const value = entry.value ?? {};
	const types = Object.keys(s.schema.backendTypes);
	const typeSchema = s.schema.backendTypes[value.type];
	const users = usersOf(name);
	const states = new Map();
	for (const f of s.facades.values()) {
		for (const c of f.candidates ?? []) {
			if (c.backend === name) {
				states.set(c.match ?? c.model, c.state);
			}
		}
	}
	const schema = typeSchema ? { ...typeSchema, properties: { ...typeSchema.properties, type: { ...typeSchema.properties.type, enum: types } } } : { properties: {} };
	return html`
		<div class="sheet">
			<${Head}
				crumbs=${[{ label: 'Backends', selection: { kind: 'backends' } }]}
				title=${html`<span class="mono">${name}</span><span class="badge">${value.type ?? 'no type'}</span>`}
				tools=${!ro && html`
					<button class="btn" disabled=${test?.name === name && test.running} title="Ask the backend for its models, with the address and key in the draft" onClick=${() => testBackend(name, setTest)}>Test</button>
					<button class="btn" onClick=${() => renameEntry('backends', name, 'backend')}>Rename</button>
					<button class="btn danger" onClick=${async () => (await deleteEntry('backends', name, users.length ? 'Its ' + plural(users.length, 'candidate') + ' would then name a backend that isn\'t defined, so the draft can\'t be applied until they change.' : '')) && select({ kind: 'backends' }, 'properties', { replace: true })}><${Icon} name="trash" /></button>
				`}
			/>
			<${EntryBanners} entry=${entry} />
			${!typeSchema && html`<${Banner} kind="warn">${value.type ? 'The gateway has no backend type ' + value.type + '; pick one of ' + types.join(', ') + '.' : 'Pick the backend\'s type.'}<//>`}
			${test?.name === name && html`
				<${Card} title="Test">
					${test.running ? html`<p class="muted">Asking ${name} for its models…</p>`
						: changedKeys(test).length > 0 ? html`<${Banner} kind="warn">${changedSince(changedKeys(test))}<//>`
						: test.ok ? html`<${Banner} kind="good">Connected: ${name} lists ${plural(test.models?.length ?? 0, 'model')}.<//>`
						: html`<${Banner}>${test.error}<//>`}
				<//>
			`}
			<${Card} title="Properties">
				<${Sheet} schema=${schema} value=${value} path=${path} readOnly=${ro} onChange=${v => {
					const done = entry.commit(v);
					recheckTest(name, value, v, done, setTest);
					return done;
				}} />
			<//>
			<${Card} title=${'Candidates on ' + name} intro=${users.length === 0 ? 'No facade uses this backend.' : null}>
				${users.length > 0 && html`
					<table class="list">
						<thead><tr><th>Facade</th><th>#</th><th>Model</th><th>Now</th></tr></thead>
						<tbody>
							${users.map(u => html`
								<tr class="row" onClick=${() => select({ kind: 'candidate', name: u.facade, index: u.index })}>
									<td class="mono">${u.facade}</td>
									<td class="num">${u.index + 1}</td>
									<td class="mono">${u.model}</td>
									<td>${states.has(u.model) ? html`<span class=${'state ' + stateClass(states.get(u.model))}>${states.get(u.model)}</span>` : html`<span class="muted">-</span>`}</td>
								</tr>
							`)}
						</tbody>
					</table>
				`}
			<//>
		</div>
	`;
}

// testBackend asks the draft's backend name for its models, once every
// field's change has reached the draft, and passes the result to setTest.
// It stays on the sheet, with what it was run on (testedState), so that a
// field changed since, and not yet changed back, marks it out of date
// (changedKeys), whether the test is still running or done.
async function testBackend(name, setTest) {
	setTest({ name, running: true });
	await settle();
	const tested = await testedState(name);
	setTest(t => (t?.name === name ? { ...t, tested, now: tested } : t));
	let result;
	try {
		result = (await request('POST', '/amf/backend-test/' + encodeURIComponent(name))).data;
	} catch (e) {
		result = { ok: false, error: errorText(e) };
	}
	setTest(t => ({ name, ...result, tested, now: t?.name === name ? t.now : tested }));
}

// testedKeys are the keys of a backend entry a test depends on: the adapter,
// and the address and key its model listing uses.
const testedKeys = ['type', 'baseUrl', 'apiKey'];

// testedState is what a test of the draft's backend name depends on, as it
// stands: type and baseUrl, and for apiKey, which the page only ever sees as
// <redacted>, its ETag, a hash of the key itself, null when there is none.
async function testedState(name) {
	const entry = get(['backends', name]) ?? {};
	return { type: entry.type, baseUrl: entry.baseUrl, apiKey: await keyTag(name) };
}

async function keyTag(name) {
	try {
		return (await request('GET', configURL(['backends', name, 'apiKey']))).etag;
	} catch (e) {
		if (e.status === 404) {
			return null;
		}
		throw e;
	}
}

// changedKeys are the testedKeys that differ now from what the shown test
// ran on.
function changedKeys(test) {
	return test.tested ? testedKeys.filter(k => test.now[k] !== test.tested[k]) : [];
}

// changedSince says which of testedKeys changed since a test.
function changedSince(keys) {
	const list = keys.length > 1 ? keys.slice(0, -1).join(', ') + ' and ' + keys.at(-1) : keys[0] ?? '';
	return list + (keys.length === 1 ? ' has' : ' have') + ' changed since this test; test again.';
}

// recheckTest updates what the shown test of backend name is compared with
// after an edit from before to after, which done settles: type and baseUrl
// at once, and apiKey, if the edit changed it, by reading its ETag once the
// edit is in the draft, so a key typed back in matches again.
function recheckTest(name, before, after, done, setTest) {
	setTest(t => (t?.name === name && t.now ? { ...t, now: { ...t.now, type: after?.type, baseUrl: after?.baseUrl } } : t));
	if (JSON.stringify(before?.apiKey) === JSON.stringify(after?.apiKey)) {
		return;
	}
	done.then(async ok => {
		if (!ok) {
			return;
		}
		const tag = await keyTag(name).catch(() => undefined);
		setTest(t => (t?.name === name && t.now ? { ...t, now: { ...t.now, apiKey: tag } } : t));
	});
}

export function stateClass(state) {
	return state === 'loaded' ? 'loaded' : state === 'starting' ? 'starting' : '';
}

// The facade sheet

function modelSchema(value) {
	const types = state.schema.modelTypes;
	return types[value?.type ?? 'candidates'] ?? types.candidates;
}

export function ModelSheet({ name }) {
	const s = useStore();
	const path = ['models', name];
	const entry = useEntry(path);
	const ro = readOnly();
	const wait = waiting(entry);
	if (wait) {
		return wait;
	}
	if (entry.missing) {
		return html`<div class="sheet"><${Banner} kind="warn">The draft has no facade named ${name}.<//></div>`;
	}
	const value = entry.value ?? {};
	const schema = modelSchema(value);
	const facade = s.facades.get(name);
	return html`
		<div class="sheet">
			<${Head}
				crumbs=${[{ label: 'Models', selection: { kind: 'models' } }]}
				title=${html`<span class="mono">${name}</span>${facade?.current && html`<span class="pill good">now ${facade.current.model}</span>`}${facade && !facade.current && html`<span class="pill bad">nothing eligible now</span>`}`}
				subtitle=${value.description}
				tools=${!ro && html`
					<button class="btn" onClick=${() => renameEntry('models', name, 'model')}>Rename</button>
					<button class="btn danger" onClick=${async () => (await deleteEntry('models', name)) && select({ kind: 'models' }, 'properties', { replace: true })}><${Icon} name="trash" /></button>
				`}
			/>
			<${EntryBanners} entry=${entry} />
			<${Card} title="Properties">
				<${Sheet} schema=${schema} value=${value} path=${path} readOnly=${ro} omit=${['candidates']} onChange=${entry.commit} />
			<//>
			<${Candidates} name=${name} entry=${entry} schema=${schema} facade=${facade} />
		</div>
	`;
}

// candidateState finds the state /amf/models reports for candidate c of a
// facade: by model and backend, since that list leaves out the candidates
// the caller may not use.
function candidateState(facade, c, facadeBackend) {
	return facade?.candidates?.find(x => x.model === c.model && x.backend === (c.backend ?? facadeBackend));
}

function Candidates({ name, entry, schema, facade }) {
	const ro = readOnly();
	const value = entry.value ?? {};
	const list = Array.isArray(value.candidates) ? value.candidates : [];
	const set = l => entry.commit(withKey(value, 'candidates', l));
	const { rowProps, buttons } = useReorder(list.length, (from, to) => set(moved(list, from, to)));
	const add = async () => {
		const c = await openDialog(close => html`<${AddCandidateDialog} facadeBackend=${value.backend} close=${close} />`);
		if (c) {
			set([...list, c]);
		}
	};
	const shared = sharing(['models', name, 'candidates']);
	return html`
		<${Card} title="Candidates" intro=${schema.properties.candidates.description} tools=${!ro && !shared && html`<button class="btn small" onClick=${add}><${Icon} name="plus" size=${14} />Add a candidate</button>`}>
			${shared && html`<div class="fields"><${SharedNote} shared=${shared} /></div>`}
			<table class="list">
				<thead><tr>${!ro && html`<th></th>`}<th>#</th><th>Model</th><th>Backend</th><th>Load if none</th><th>Params</th><th>Now</th><th></th></tr></thead>
				<tbody>
					${list.length === 0 && html`<tr><td colspan="8" class="empty">No candidates, so the draft can't be applied until there is one.</td></tr>`}
					${list.map((c, i) => {
						const st = candidateState(facade, c, value.backend);
						const paramsShared = sharing(['models', name, 'candidates', String(i), 'params']);
						return html`
							<tr key=${i + ':' + c.model} ...${rowProps(i)} onClick=${() => select({ kind: 'candidate', name, index: i })}>
								${!ro && html`<td class="grip" title="Drag to reorder"><${Icon} name="grip" /></td>`}
								<td class="num">${i + 1}</td>
								<td class="mono">${c.model}${st?.match && st.match !== c.model && html` <span class="muted">→ ${st.match}</span>`}</td>
								<td class="mono">${c.backend ?? html`<span class="muted">${value.backend ?? '-'}</span>`}</td>
								<td>${c.loadIfNone && html`<span class="pill accent plain">loadIfNone</span>`}</td>
								<td>${c.params !== undefined && (paramsShared ? html`<span class="badge shared">${'*' + paramsShared.anchor.name}</span>` : html`<span class="badge">set</span>`)}</td>
								<td>${st ? html`<span class=${'state ' + stateClass(st.state) + (st.winner ? ' winner' : '')}>${st.winner ? 'wins: ' : ''}${st.state}</span>` : html`<span class="muted">-</span>`}</td>
								<td class="order">
									${buttons(i)}
									${!ro && html`<button class="btn ghost icon" title="Remove" onClick=${e => (e.stopPropagation(), set(list.filter((_, j) => j !== i)))}><${Icon} name="trash" /></button>`}
								</td>
							</tr>
						`;
					})}
				</tbody>
			</table>
		<//>
	`;
}

function AddCandidateDialog({ facadeBackend, close }) {
	const s = useStore();
	const backends = keysOf(s.draft.backends ?? {});
	const [model, setModel] = useState('');
	const [backend, setBackend] = useState('');
	const [loadIfNone, setLoadIfNone] = useState(false);
	const ok = model.trim() !== '' && (backend || facadeBackend);
	const submit = () => {
		if (!ok) {
			return;
		}
		const c = { model: model.trim() };
		if (backend) {
			c.backend = backend;
		}
		if (loadIfNone) {
			c.loadIfNone = true;
		}
		close(c);
	};
	return html`
		<${Dialog} title="Add a candidate" foot=${html`<button class="btn" onClick=${() => close()}>Cancel</button><button class="btn primary" disabled=${!ok} onClick=${submit}>Add</button>`}>
			<label class="stack">Model: an ID or alias the backend lists, or a pattern such as qwen3:*<input type="text" value=${model} ref=${focusOnMount} onInput=${e => setModel(e.target.value)} onKeyDown=${e => e.key === 'Enter' && submit()} /></label>
			<label class="stack">Backend<select value=${backend} onChange=${e => setBackend(e.target.value)}>
				<option value="">${facadeBackend ? 'the facade\'s, ' + facadeBackend : '(pick one)'}</option>
				${backends.map(b => html`<option value=${b}>${b}</option>`)}
			</select></label>
			<label class="stack" style="flex-direction:row;align-items:center;gap:8px"><input type="checkbox" checked=${loadIfNone} onChange=${e => setLoadIfNone(e.target.checked)} />Load it if no candidate is loaded or loading</label>
			<p>It goes last in the list; drag it to where it belongs.</p>
		<//>
	`;
}

// The candidate sheet

export function CandidateSheet({ name, index }) {
	const s = useStore();
	const path = ['models', name];
	const entry = useEntry(path);
	const ro = readOnly();
	const value = entry.value ?? {};
	const list = Array.isArray(value.candidates) ? value.candidates : [];
	const wait = waiting(entry);
	if (wait) {
		return wait;
	}
	if (entry.missing || !list[index]) {
		return html`<div class="sheet"><${Banner} kind="warn">The draft has no candidate ${index + 1} in ${name}.<//></div>`;
	}
	const c = list[index] ?? {};
	const schema = modelSchema(value).properties.candidates.items;
	const st = candidateState(s.facades.get(name), c, value.backend);
	const set = l => entry.commit(withKey(value, 'candidates', l));
	return html`
		<div class="sheet">
			<${Head}
				crumbs=${[{ label: 'Models', selection: { kind: 'models' } }, { label: name, selection: { kind: 'model', name } }]}
				title=${html`<span class="muted">${'#' + (index + 1)}</span><span class="mono">${c.model}</span>${st && html`<span class=${'pill ' + (st.winner ? 'good' : st.state === 'loaded' ? 'good' : st.state === 'starting' ? 'warn' : '')}>${st.winner ? 'wins now' : st.state}</span>`}`}
				subtitle=${st?.match && st.match !== c.model ? 'Matches ' + st.match + ' now.' : null}
				tools=${html`
					<button class="btn icon" title="Previous candidate" disabled=${index === 0} onClick=${() => select({ kind: 'candidate', name, index: index - 1 })}><${Icon} name="up" /></button>
					<button class="btn icon" title="Next candidate" disabled=${index === list.length - 1} onClick=${() => select({ kind: 'candidate', name, index: index + 1 })}><${Icon} name="down" /></button>
					${!ro && html`<button class="btn danger" onClick=${async () => {
						if (await confirm({ title: 'Remove candidate ' + (index + 1), body: c.model + ' leaves ' + name + '\'s candidates in the draft.', yes: 'Remove', danger: true })) {
							await set(list.filter((_, j) => j !== index));
							select({ kind: 'model', name }, 'properties', { replace: true });
						}
					}}><${Icon} name="trash" /></button>`}
				`}
			/>
			<${EntryBanners} entry=${entry} />
			<${Card} title="Properties">
				<${Sheet} schema=${schema} value=${c} path=${[...path, 'candidates', String(index)]} readOnly=${ro} onChange=${x => set(list.map((y, j) => (j === index ? x : y)))} />
			<//>
		</div>
	`;
}
