// The admin UI: sign-in, the bar with the state of the config and what can
// be done with it, the tree of the config, and the main pane, which shows
// the selection's sheet, its YAML or the anchors, above or in place of the
// log.

import { render } from 'preact';
import { useEffect, useRef, useState } from './vendor/hooks.mjs';
import { ApiError, currentKey, entriesOf, hasKey, isObject, request, signIn, signOut } from './api.mjs';
import { keyHash } from './sha256.mjs';
import { errorText, followHistory, forgetKeys, freshKey, load, refresh, refreshFacades, select, selectionFromHash, set, settle, state, useStore, watch } from './store.mjs';
import { confirm, Dialog, Dialogs, html, Icon, openDialog, plural, Reveal, toast, useNarrow } from './ui.mjs';
import { BackendSheet, CandidateSheet, ModelSheet, SectionList, ServerSheet, stateClass } from './sheets.mjs';
import { DiffDialog, VarsEditor, YAMLEditor } from './editors.mjs';
import { LogPanel, useLogMode } from './log.mjs';

// Sign-in

function SignIn() {
	const s = useStore();
	const [key, setKey] = useState('');
	const [remember, setRemember] = useState(false);
	const [busy, setBusy] = useState(false);
	const submit = async e => {
		e.preventDefault();
		setBusy(true);
		signIn(key.trim(), remember);
		await start();
		setBusy(false);
	};
	return html`
		<div class="signin">
			<form onSubmit=${submit}>
				<h1><span class="brand"><span class="logo">MF</span></span>ai-model-facade</h1>
				<p>Sign in with a key that <code>server.adminApi</code> lists, to read and change this gateway's config.</p>
				<div class="host">${location.host}</div>
				${s.signInError && html`<div class="banner bad" role="alert">${s.signInError}</div>`}
				<input type="text" name="username" autocomplete="username" value="admin" hidden />
				<label>Key
					<${Reveal} field=${hidden => html`<input type=${hidden ? 'password' : 'text'} name="password" autocomplete="current-password" value=${key} required ref=${el => el && !el.dataset.focused && (el.dataset.focused = '1', el.focus())} onInput=${e => setKey(e.target.value)} />`} />
				</label>
				<label class="inline"><input type="checkbox" checked=${remember} onChange=${e => setRemember(e.target.checked)} />Stay signed in on this browser</label>
				<button class="btn primary" style="justify-content:center;height:36px" disabled=${busy || !key.trim()}>Sign in</button>
				<p class="muted">The key is kept in this browser only, and sent to this gateway alone.</p>
			</form>
		</div>
	`;
}

// start loads the UI with the key signed in with, or with none, for a
// gateway that lets callers without a key use its admin API.
async function start() {
	set({ phase: 'loading', signInError: null });
	try {
		await load();
		refreshFacades();
	} catch (e) {
		let message = errorText(e);
		if (e instanceof ApiError) {
			if (e.status === 401) {
				message = 'The gateway doesn\'t accept this key.';
			} else if (e.status === 403) {
				message = 'The gateway accepts this key, but server.adminApi doesn\'t list it: ' + e.message;
			} else if (e.status === 404) {
				message = 'The admin API is off on this gateway: ' + e.message;
			}
		}
		signOut();
		set({ phase: 'signin', signInError: hadKey ? message : null });
	}
	hadKey = true;
}

let hadKey = hasKey();

// The bar

function TopBar({ logMode, setLogMode, onMenu, barRef }) {
	const s = useStore();
	const st = s.status;
	const write = s.caller.write;
	const changedOnDisk = st.files.filter(f => f.changedOnDisk);
	const unsaveable = st.files.filter(f => !f.saveable);
	return html`
		<header class="topbar" ref=${barRef}>
			<div class="bar-row">
			<button class="btn ghost icon nav-toggle" title="The config's tree" aria-label="Open the config's tree" onClick=${onMenu}><${Icon} name="menu" /></button>
			<div class="brand"><span class="logo">MF</span>ai-model-facade</div>
			<div class="states">
				${s.newVersion && !s.unreachable && html`<button class="pill accent" title=${'The gateway now runs ' + s.newVersion + '; this page was loaded from ' + s.loadedVersion + '. Reload to use the page that comes with it.'} onClick=${() => location.reload()}>new version: reload</button>`}
				${s.unreachable ? html`<${PillNote} kind="bad" label="can't reach the gateway" title="The gateway can't be reached" lines=${[s.unreachable.error, 'The page shows the config and the facades as they last were, and tries again in ' + s.unreachable.retry + ' s.']} />` : html`
				${st.draft.live ? html`<span class="pill good" title="The draft holds what is running">config live</span>` : html`<span class="pill accent" title=${'Draft from ' + st.draft.source + (st.draft.edited ? ', edited since' : '') + '; not applied yet'}>changes staged</span>`}
				${st.live.saved ? html`<span class="pill good" title="The running config is what the files hold">saved</span>` : html`<span class="pill warn" title=${'Running config from ' + st.live.source + (st.live.edited ? ', edited since' : '')}>running config not saved</span>`}
				${changedOnDisk.map(f => html`<span class="pill warn" title=${f.path + ' changed on disk since the gateway read it; Load the current save to see it, or Reload to run it.'}>${f.role} changed on disk</span>`)}
				${(st.restart ?? []).length > 0 && html`<${PillNote} kind="warn" label="restart needed" title="A restart is needed" lines=${restartLines(st.restart)} />`}
				${unsaveable.length > 0 && html`<${PillNote} kind="bad" label="can't save" title="Why the files can't be saved" lines=${unsaveable.map(f => f.role + ' (' + f.path + '): ' + f.reason)} />`}
				${!write && html`<span class="pill plain">read only</span>`}
				`}
			</div>
			</div>
			<div class="actions-wrap">
			<div class="actions">
				<button class=${'btn' + (st.draft.live ? ' dim' : '')} title="What applying the draft would change" onClick=${async () => {
					await settle();
					openDialog(close => html`<${DiffDialog} close=${close} />`);
				}}><${Icon} name="diff" />Diff</button>
				${write && html`
					<button class="btn" disabled=${st.draft.live} onClick=${revert}>Revert</button>
					<button class="btn primary" disabled=${st.draft.live} onClick=${apply}>Apply</button>
					<button class="btn" disabled=${st.live.saved || unsaveable.length > 0} title=${unsaveable.length > 0 ? unsaveable.map(f => f.role + ': ' + f.reason).join('\n') : 'Write the running config to the files'} onClick=${() => save(false)}>Save</button>
					<${More} />
				`}
				<button class=${'btn' + (logMode !== 'hidden' ? ' primary' : '')} title="The gateway's log" onClick=${() => setLogMode(logMode === 'hidden' ? 'docked' : 'hidden')}><${Icon} name="log" />Log</button>
				<span class="who" title="The key signed in with">${s.caller.name || 'no key'}</span>
				${hasKey() && html`<button class="btn ghost small" onClick=${() => {
					signOut();
					set({ phase: 'signin', signInError: null });
				}}>Sign out</button>`}
			</div>
			</div>
		</header>
	`;
}

// PillNote is a pill whose details show in its tooltip, and in a dialog on
// a click or a tap, since a phone has no hover.
function PillNote({ kind, label, title, lines }) {
	return html`<button class=${'pill ' + kind} title=${lines.join('\n\n')} onClick=${() => openDialog(close => html`
		<${Dialog} title=${title} foot=${html`<button class="btn primary" onClick=${() => close()}>OK</button>`}>
			${lines.map(l => html`<p>${l}</p>`)}
		<//>
	`)}>${label}</button>`;
}

// restartLines says what a restart would change of where the gateway
// listens, and of where it serves the admin UI, and what to know until
// then.
function restartLines(pending) {
	const where = (key, address) => (address ? address : key === 'server.adminApi.listen' ? 'server.listen, with the client APIs' : 'nowhere');
	const lines = pending.map(p => p.key === 'server.adminApi.uiBasePath'
		? p.key + ': the admin UI is at ' + p.listening + '; the running config says ' + p.running + ', which waits for server.adminApi.listen.'
		: p.key + ': listening on ' + where(p.key, p.listening) + '; the running config says ' + where(p.key, p.running) + '.');
	lines.push('The gateway goes on listening where it started until it restarts, since those addresses are bound.');
	if (pending.some(p => !p.saved)) {
		lines.push('The files don\'t hold the new address yet, so save the running config before restarting; a restart before that starts from the files and loses it.');
	}
	if (pending.some(p => p.saved && p.key === 'server.listen')) {
		lines.push('The files hold the new address, so the health check, Docker\'s included, probes it already and reports the gateway unhealthy until it restarts.');
	}
	lines.push('If the gateway runs in a container and a port changed, publish the new port too, or the restarted gateway listens where nothing forwards to.');
	return lines;
}

function More() {
	const [open, setOpen] = useState(false);
	useEffect(() => {
		if (!open) {
			return;
		}
		const close = () => setOpen(false);
		setTimeout(() => addEventListener('click', close), 0);
		return () => removeEventListener('click', close);
	}, [open]);
	return html`
		<span style="position:relative">
			<button class="btn" title="More" onClick=${() => setOpen(!open)}>⋯</button>
			${open && html`
				<div class="card" style="position:absolute;right:0;top:34px;z-index:10;min-width:260px;padding:6px;display:flex;flex-direction:column;gap:2px">
					<button class="btn ghost" style="justify-content:flex-start" onClick=${loadFiles}>Load the current save</button>
					<button class="btn ghost" style="justify-content:flex-start" onClick=${() => select({ kind: 'server' }, 'properties', { scrollTo: 'backups' })}>Load a backup...</button>
					<button class="btn ghost danger" style="justify-content:flex-start" onClick=${reload}>Reload: load and run the current save</button>
				</div>
			`}
		</span>
	`;
}

function results(r) {
	return [...(r.warnings ?? []).map(w => 'Warning: ' + w), ...(r.notes ?? [])].join('\n');
}

// ownKey says what applying the draft does to the key the page signed in
// with: nothing; replaces it with a key this page made or was given for the
// same name, which the page then signs in with; or drops it.
function ownKey() {
	const { key, remembered } = currentKey();
	const listed = state.draft?.server?.apiKeys ?? [];
	if (!key || listed.some(k => k.hash === keyHash(key))) {
		return null;
	}
	const successor = listed.find(k => k.name === state.caller?.name && k.hash && freshKey(k.hash));
	return successor ? { next: freshKey(successor.hash), remembered } : { next: null };
}

async function apply() {
	await settle();
	const own = ownKey();
	const notice = own && (own.next
		? 'The draft replaces the key this page is signed in with; the page signs in with the new key once the draft is applied.'
		: 'The draft no longer holds the key this page is signed in with, so the page asks you to sign in again once the draft is applied.');
	const etag = await openDialog(close => html`<${DiffDialog} close=${close} onApply notice=${notice} />`);
	if (!etag) {
		return;
	}
	try {
		const r = await request('POST', '/amf/config-apply', { ifMatch: etag });
		if (own?.next) {
			signIn(own.next, own.remembered);
		}
		forgetKeys(); // applied, so the keys made for it are no longer shown
		try {
			await refresh();
		} catch (e) {
			if (e instanceof ApiError && (e.status === 401 || e.status === 403)) {
				signOut();
				set({ phase: 'signin', signInError: 'The applied config no longer lets the key this page signed in with use the admin API; sign in with one it lists.' });
				return;
			}
			throw e;
		}
		refreshFacades();
		const more = results(r.data);
		toast('Applied: ' + plural(r.data.models, 'facade') + ' running.' + (more ? '\n' + more : ''));
	} catch (e) {
		await refresh();
		openDialog(close => html`<${Dialog} title="The draft wasn't applied" foot=${html`<button class="btn primary" onClick=${() => close()}>OK</button>`}><p>The running config is unchanged.</p><div class="banner bad">${errorText(e)}</div><//>`);
	}
}

async function revert() {
	await settle();
	if (!(await confirm({ title: 'Revert the draft', body: 'The draft goes back to the running config, and its edits are lost.', yes: 'Revert', danger: true }))) {
		return;
	}
	try {
		await request('POST', '/amf/config-revert');
		await refresh();
	} catch (e) {
		toast(errorText(e));
	}
}

async function save(force) {
	await settle();
	const st = state.status;
	if (!force && !(await confirm({
		title: 'Save the running config',
		body: html`<p>The running config is written to the files that differ from it:</p><ul style="margin:0">${st.files.map(f => html`<li><code>${f.path}</code></li>`)}</ul><p>${st.backups?.keep === 0 ? 'Both files are copied into a backup first, which is removed once the save succeeds.' : 'Both files are copied into a backup first, in ' + st.backups?.path + '.'}</p>${!st.draft.live && html`<p>The draft isn't saved: only what is running is.</p>`}`,
		yes: 'Save',
	}))) {
		return;
	}
	try {
		const r = await request('POST', '/amf/config-save', { query: force ? { force: '' } : {} });
		await refresh();
		toast((r.data.saved.length ? 'Saved ' + r.data.saved.join(' and ') : 'Nothing to write: the files already hold it') + (r.data.backup ? ', after backup ' + r.data.backup : '') + '.' + (r.data.notes.length ? '\n' + r.data.notes.join('\n') : ''));
	} catch (e) {
		if (e instanceof ApiError && e.status === 409 && !force && /changed on disk/.test(e.message)) {
			if (await confirm({ title: 'A file changed on disk', body: html`<div class="banner warn">${e.message}</div><p>Saving anyway overwrites that change. The backup the save makes first keeps it.</p>`, yes: 'Save anyway', danger: true })) {
				save(true);
			}
			return;
		}
		toast(errorText(e));
	}
}

async function loadFiles() {
	await settle();
	if (state.status.draft.edited && !(await confirm({ title: 'Replace the draft', body: 'The draft holds edits that aren\'t applied. Loading the files replaces them.', yes: 'Load', danger: true }))) {
		return;
	}
	try {
		const r = await request('POST', '/amf/config-load');
		await refresh();
		toast('Loaded the current save into the draft.' + (r.data.notes.length ? '\n' + r.data.notes.join('\n') : ''));
	} catch (e) {
		toast(errorText(e));
	}
}

async function reload() {
	await settle();
	const st = state.status;
	const lost = [!st.draft.live && 'the draft\'s edits', !st.live.saved && 'the running config\'s unsaved changes'].filter(Boolean);
	if (!(await confirm({ title: 'Reload the config', body: 'The gateway loads and runs the current save now, as SIGHUP does, and the draft becomes a copy of it.' + (lost.length ? ' This discards ' + lost.join(' and ') + '.' : ''), yes: 'Reload', danger: lost.length > 0 }))) {
		return;
	}
	try {
		const r = await request('POST', '/amf/reload');
		await refresh();
		refreshFacades();
		const more = results(r.data);
		toast('Reloaded: ' + plural(r.data.models, 'facade') + ' running.' + (more ? '\n' + more : ''));
	} catch (e) {
		toast(errorText(e));
	}
}

// The tree

function Twisty({ open, onToggle }) {
	return html`<button class=${'twisty' + (open ? ' open' : '')} tabindex="-1" onClick=${e => (e.stopPropagation(), onToggle())}><${Icon} name="chevron" size=${12} /></button>`;
}

function Row({ depth, selected, onClick, children, twisty }) {
	return html`
		<div class=${'tree-row' + (selected ? ' selected' : '')} style=${'padding-left:' + (8 + depth * 16) + 'px'} role="treeitem" aria-selected=${selected} onClick=${onClick}>
			${twisty ?? html`<span class="twisty"></span>`}${children}
		</div>
	`;
}

function same(a, b) {
	return a.kind === b.kind && a.name === b.name && a.index === b.index;
}

// Tree is the config's tree, in a pane of its own, or on a narrow screen in a
// drawer, open while open is, which a pick closes (onPick).
function Tree({ open, onPick }) {
	const s = useStore();
	const [filter, setFilter] = useState('');
	const [closed, setClosed] = useState(new Set());
	const toggle = id => {
		const c = new Set(closed);
		c.has(id) ? c.delete(id) : c.add(id);
		setClosed(c);
	};
	const sel = s.selection;
	const go = selection => {
		select(selection, s.view === 'vars' ? 'properties' : s.view);
		onPick();
	};
	const needle = filter.trim().toLowerCase();
	const matches = name => !needle || name.toLowerCase().includes(needle);
	const backends = entriesOf(isObject(s.draft.backends) ? s.draft.backends : {});
	const models = entriesOf(isObject(s.draft.models) ? s.draft.models : {});
	const shownModels = models.filter(([name, m]) => matches(name) || (m?.candidates ?? []).some(c => matches(String(c.model))));
	// The button at the end of the Server row folds the tree a level at a
	// time: the open facades first, then the two sections, and once all is
	// folded, opens it all again. A facade counts as open only while Models
	// is, so a click always changes what is on screen.
	const facadesOpen = !closed.has('m') && models.some(([name, m]) => Array.isArray(m?.candidates) && m.candidates.length > 0 && !closed.has('m:' + name));
	const sectionsOpen = !closed.has('b') || !closed.has('m');
	const fold = facadesOpen
		? { icon: 'collapse', title: 'Fold the facades', set: () => new Set([...closed, ...models.map(([name]) => 'm:' + name)]) }
		: sectionsOpen
			? { icon: 'collapse', title: 'Fold Backends and Models', set: () => new Set([...closed, 'b', 'm']) }
			: { icon: 'expand', title: 'Open everything', set: () => new Set() };
	return html`
		<aside class=${'tree-pane' + (open ? ' open' : '')}>
			<div class="filter"><input type="search" placeholder="Filter" value=${filter} onInput=${e => setFilter(e.target.value)} /></div>
			<div class="tree" role="tree">
				<${Row} depth=${0} selected=${same(sel, { kind: 'server' })} onClick=${() => go({ kind: 'server' })}>
					<span class="label">Server</span>
					<button class="btn ghost icon tree-fold" title=${fold.title} aria-label=${fold.title} onClick=${e => (e.stopPropagation(), setClosed(fold.set()))}><${Icon} name=${fold.icon} size=${14} /></button>
				<//>
				<${Row} depth=${0} selected=${same(sel, { kind: 'backends' })} onClick=${() => go({ kind: 'backends' })} twisty=${html`<${Twisty} open=${!closed.has('b')} onToggle=${() => toggle('b')} />`}>
					<span class="label section">Backends</span><span class="count">${backends.length}</span>
				<//>
				${!closed.has('b') && backends.filter(([name]) => matches(name)).map(([name, b]) => html`
					<${Row} depth=${1} selected=${same(sel, { kind: 'backend', name })} onClick=${() => go({ kind: 'backend', name })}>
						<span class="label mono">${name}</span><span class="badge">${b?.type}</span>
					<//>
				`)}
				<${Row} depth=${0} selected=${same(sel, { kind: 'models' })} onClick=${() => go({ kind: 'models' })} twisty=${html`<${Twisty} open=${!closed.has('m')} onToggle=${() => toggle('m')} />`}>
					<span class="label section">Models</span><span class="count">${models.length}</span>
				<//>
				${!closed.has('m') && shownModels.map(([name, m]) => {
					const facade = s.facades.get(name);
					const candidates = Array.isArray(m?.candidates) ? m.candidates : [];
					const open = !closed.has('m:' + name) || (needle && !matches(name));
					return html`
						<${Row} depth=${1} selected=${same(sel, { kind: 'model', name })} onClick=${() => go({ kind: 'model', name })} twisty=${candidates.length > 0 && html`<${Twisty} open=${open} onToggle=${() => toggle('m:' + name)} />`}>
							<span class=${'dot ' + (facade?.current ? 'loaded' : '')} title=${facade?.current ? 'Now: ' + facade.current.model : facade ? 'Nothing eligible now' : ''}></span>
							<span class="label mono">${name}</span>
						<//>
						${open && candidates.map((c, i) => {
							const st = facade?.candidates?.find(x => x.model === c.model && x.backend === (c.backend ?? m.backend));
							return (!needle || matches(name) || matches(String(c.model))) && html`
								<${Row} depth=${2} selected=${same(sel, { kind: 'candidate', name, index: i })} onClick=${() => go({ kind: 'candidate', name, index: i })}>
									<span class=${'dot ' + stateClass(st?.state)} title=${st ? st.state : ''}></span>
									<span class="label mono" style=${st?.winner ? 'font-weight:650' : ''}>${c.model}</span>
								<//>
							`;
						})}
					`;
				})}
			</div>
		</aside>
	`;
}

// The main pane

function Content() {
	const s = useStore();
	const sel = s.selection;
	const crumbs = {
		server: [],
		backends: [],
		backend: [{ label: 'Backends', selection: { kind: 'backends' } }],
		models: [],
		model: [{ label: 'Models', selection: { kind: 'models' } }],
		candidate: [{ label: 'Models', selection: { kind: 'models' } }, { label: sel.name, selection: { kind: 'model', name: sel.name } }],
	}[sel.kind];
	if (s.view === 'vars') {
		return html`<${VarsEditor} crumbs=${crumbs} />`;
	}
	if (s.view === 'yaml') {
		const yaml = {
			server: [['server'], 'Server'],
			backends: [['backends'], 'Backends'],
			backend: [['backends', sel.name], sel.name],
			models: [['models'], 'Models'],
			model: [['models', sel.name], sel.name],
			candidate: [['models', sel.name], sel.name],
		}[sel.kind];
		return html`<${YAMLEditor} key=${yaml[0].join('/')} path=${yaml[0]} title=${yaml[1]} crumbs=${crumbs} />`;
	}
	switch (sel.kind) {
	case 'backends':
		return html`<${SectionList} key="backends" section="backends" />`;
	case 'models':
		return html`<${SectionList} key="models" section="models" />`;
	case 'backend':
		return html`<${BackendSheet} key=${'b:' + sel.name} name=${sel.name} />`;
	case 'model':
		return html`<${ModelSheet} key=${'m:' + sel.name} name=${sel.name} />`;
	case 'candidate':
		return html`<${CandidateSheet} key=${'c:' + sel.name + ':' + sel.index} name=${sel.name} index=${sel.index} />`;
	}
	return html`<${ServerSheet} />`;
}

function Ready() {
	const [logMode, setLogMode] = useLogMode();
	const [treeOpen, setTreeOpen] = useState(false);
	const narrow = useNarrow();
	useEffect(() => watch(), []);
	useEffect(() => followHistory(), []);
	useEffect(() => {
		if (!treeOpen) {
			return;
		}
		const onKey = e => e.key === 'Escape' && setTreeOpen(false);
		addEventListener('keydown', onKey);
		return () => removeEventListener('keydown', onKey);
	}, [treeOpen]);
	// A narrow screen has no room for the log below the sheet, so it takes
	// the entire screen while it is open.
	const shownMode = narrow && logMode === 'docked' ? 'full' : logMode;
	const bar = useRef();
	const content = useRef();
	useScrollAway(narrow, shownMode !== 'full', bar, content);
	return html`
		<div class="app">
			<${TopBar} logMode=${logMode} setLogMode=${setLogMode} onMenu=${() => setTreeOpen(true)} barRef=${bar} />
			<div class="body">
				<${Tree} open=${narrow && treeOpen} onPick=${() => setTreeOpen(false)} />
				${narrow && treeOpen && html`<div class="drawer-backdrop" onClick=${() => setTreeOpen(false)}></div>`}
				<main class=${'main' + (shownMode === 'full' ? ' log-full' : '')}>
					<div class="content" ref=${content}><${Content} /></div>
					<${LogPanel} mode=${shownMode} setMode=${setLogMode} narrow=${narrow} />
				</main>
			</div>
		</div>
	`;
}

// useScrollAway, on a narrow screen, where the bar is a fixed row with the
// buttons on rows below it, keeps --bar-height at the height of it all,
// which the panes below start at, and --bar-row-height at the height of
// its first row. While moving is on, it moves the bar's
// buttons and then the sheet's name row, which sticks below them, out of
// the way as the sheet scrolls: up under the first row by as much as the
// sheet scrolls down, and back by as much as it scrolls back, the name row
// first, so they are shown again the moment the sheet scrolls up,
// wherever it is.
function useScrollAway(narrow, moving, bar, content) {
	useEffect(() => {
		const header = bar.current;
		if (!narrow || !header) {
			return;
		}
		const app = header.parentElement;
		const row = header.querySelector('.bar-row');
		const wrap = header.querySelector('.actions-wrap');
		const buttons = header.querySelector('.actions');
		// The bar's height with its buttons all shown, whatever part of them
		// is away now, and with them all away.
		const measure = () => {
			app.style.setProperty('--bar-height', header.offsetHeight - wrap.offsetHeight + buttons.offsetHeight + 'px');
			app.style.setProperty('--bar-row-height', header.offsetHeight - wrap.offsetHeight + 'px');
		};
		measure();
		const resize = new ResizeObserver(measure);
		resize.observe(row);
		resize.observe(buttons);
		return () => {
			resize.disconnect();
			app.style.removeProperty('--bar-height');
			app.style.removeProperty('--bar-row-height');
		};
	}, [narrow]);
	useEffect(() => {
		const header = bar.current;
		const pane = content.current;
		if (!narrow || !moving || !header || !pane) {
			return;
		}
		const wrap = header.querySelector('.actions-wrap');
		const buttons = header.querySelector('.actions');
		let last = Math.max(0, pane.scrollTop);
		let away = 0;
		const place = () => {
			const shown = buttons.offsetHeight;
			const buttonsAway = Math.min(away, shown);
			// Clipped only while partly away, so the ... menu can open below.
			wrap.style.height = buttonsAway ? shown - buttonsAway + 'px' : '';
			wrap.style.overflow = buttonsAway ? 'hidden' : '';
			buttons.style.transform = buttonsAway ? 'translateY(' + -buttonsAway + 'px)' : '';
			// The sheet changes with the selection, so its row is found each
			// time. Its sticky top is measured from the pane's top padding,
			// the bar with all its buttons, so it is up by all that is away.
			const name = pane.querySelector('.head-row');
			if (name) {
				name.style.top = away ? -away + 'px' : '';
			}
		};
		const onScroll = () => {
			const top = Math.max(0, pane.scrollTop); // a bounce past the top reads negative
			const name = pane.querySelector('.head-row');
			const most = buttons.offsetHeight + (name ? name.offsetHeight : 0);
			away = top === 0 ? 0 : Math.min(most, Math.max(0, away + top - last));
			last = top;
			place();
		};
		pane.addEventListener('scroll', onScroll, { passive: true });
		return () => {
			pane.removeEventListener('scroll', onScroll);
			away = 0;
			place();
		};
	}, [narrow, moving]);
}

function App() {
	const s = useStore();
	return html`
		${s.phase === 'signin' && html`<${SignIn} />`}
		${s.phase === 'loading' && html`<div class="signin"><p class="muted">Connecting to the gateway...</p></div>`}
		${s.phase === 'ready' && html`<${Ready} />`}
		<${Dialogs} />
	`;
}

const initial = selectionFromHash();
set({ selection: initial.selection, view: initial.view });
render(html`<${App} />`, document.body);
start();
