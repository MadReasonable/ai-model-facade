// The text views: the YAML of a node, the anchors under vars, and the
// difference between the draft and the running config.

import { useEffect, useState } from './vendor/hooks.mjs';
import { configURL, request } from './api.mjs';
import { edit, errorText, select, state, useStore } from './store.mjs';
import { Banner, Dialog, html, pathText } from './ui.mjs';
import { Head } from './sheets.mjs';
import { CodeArea } from './code.mjs';

// useYAML reads the node at path as YAML, comments and anchors included,
// again each time the draft changes; text is undefined while it loads and
// null if there is no such node.
function useYAML(path) {
	const s = useStore();
	const key = path.join('\u0000');
	const [node, setNode] = useState({});
	useEffect(() => {
		let current = true;
		request('GET', configURL(path), { yaml: true }).then(
			r => current && setNode({ text: r.data, etag: r.etag }),
			e => current && setNode(e.status === 404 ? { text: null, etag: null } : { error: errorText(e) }),
		);
		return () => {
			current = false;
		};
	}, [key, s.draftETag]);
	return node;
}

// YAMLEditor edits the node at path as the files hold it. A save replaces
// the node, if no one changed it since it was read.
export function YAMLEditor({ path, title, crumbs, subtitle, aside }) {
	return html`
		<div class="sheet">
			<${Head} crumbs=${crumbs} title=${title} subtitle=${subtitle ?? html`The YAML of <code>${pathText(path)}</code> in the draft, as the files hold it, secrets shown as <code>${'<redacted>'}</code>, which a save keeps.`} />
			${aside ? html`<div class="vars-layout"><${YAMLBody} path=${path} />${aside}</div>` : html`<${YAMLBody} path=${path} />`}
		</div>
	`;
}

// YAMLBody is the editor of a YAMLEditor: the text, and its buttons.
function YAMLBody({ path }) {
	const node = useYAML(path);
	const ro = !state.caller?.write;
	const [text, setText] = useState(null);
	const [error, setError] = useState(null);
	const [saving, setSaving] = useState(false);
	useEffect(() => {
		setText(null);
		setError(null);
	}, [path.join('\u0000')]);
	const shown = text ?? node.text ?? '';
	const dirty = text !== null && text !== (node.text ?? '');
	const save = async () => {
		setSaving(true);
		setError(null);
		try {
			await edit(path, shown, node.text === null ? null : node.etag, { yaml: true });
			setText(null);
		} catch (e) {
			setError(errorText(e));
		}
		setSaving(false);
	};
	return html`
		<div class="editor">
			${node.error && html`<${Banner}>${node.error}<//>`}
			${error && html`<${Banner} onClose=${() => setError(null)}>${error}<//>`}
			<${CodeArea} lang="yaml" disabled=${ro || node.text === undefined} value=${shown} onInput=${e => setText(e.target.value)} onKeyDown=${e => {
				if ((e.ctrlKey || e.metaKey) && e.key === 's') {
					e.preventDefault();
					dirty && save();
				}
			}} />
			<div class="bar">
				<span class="grow">${node.text === null ? 'There is no such node in the draft yet; a save adds it.' : dirty ? 'Changed here, not yet in the draft.' : 'The same as the draft.'}</span>
				<button class="btn" disabled=${!dirty} onClick=${() => {
					setText(null);
					setError(null);
				}}>Undo</button>
				${!ro && html`<button class="btn primary" disabled=${!dirty || saving} onClick=${save}>Save to the draft</button>`}
			</div>
		</div>
	`;
}

// VarsEditor edits vars, the home of the anchors that entries share, with
// the list of every anchor and the values that read it beside it, on the
// right, or above it on a narrow screen.
export function VarsEditor({ crumbs }) {
	const s = useStore();
	const named = s.anchors.filter(a => a.name);
	const aside = html`
		<aside class="anchor-list">
			${named.length === 0 && html`<p class="muted">No anchors yet. Define one under vars, such as <code>noThinking: &noThinking</code>, and use it elsewhere as <code>*noThinking</code>, or merge a map with <code>${'<<: *name'}</code>.</p>`}
			${named.map(a => html`
				<div class="anchor">
					<div class="name">&${a.name}</div>
					<div class="muted mono" style="font-size:12px">${pathText(a.path)}</div>
					<ul>
						${a.users.length === 0 && html`<li class="muted">used nowhere</li>`}
						${a.users.map(u => html`<li><button onClick=${() => goTo(u.path)}>${u.via === 'merge' ? '<< ' : '* '}${pathText(u.path)}</button></li>`)}
					</ul>
				</div>
			`)}
		</aside>
	`;
	return html`<${YAMLEditor} path=${['vars']} title="&vars" crumbs=${crumbs} aside=${aside} subtitle=${html`The anchors that entries share. A value defined here as <code>name: &name</code> is read wherever <code>*name</code> stands, and a map is merged into an entry with <code>${'<<: *name'}</code>, so a change here changes every user of it. The gateway reads nothing under vars itself.`} />`;
}

// goTo selects what holds the value at path.
function goTo(path) {
	const [section, name, sub, index] = path;
	if (section === 'backends' && name !== undefined) {
		select({ kind: 'backend', name }, 'properties');
	} else if (section === 'models' && name !== undefined) {
		select(sub === 'candidates' && index !== undefined ? { kind: 'candidate', name, index: Number(index) } : { kind: 'model', name }, 'properties');
	} else {
		select({ kind: 'server' }, 'properties');
	}
}

// diffLines returns the edit from a to b, lists of lines, as a list of
// [op, line] with op ' ', '-' or '+': Myers' algorithm, so it takes time in
// proportion to the size of the files and of the difference.
export function diffLines(a, b) {
	const n = a.length;
	const m = b.length;
	const max = n + m;
	const v = new Int32Array(2 * max + 2);
	const trace = [];
	for (let d = 0; d <= max; d++) {
		trace.push(v.slice());
		for (let k = -d; k <= d; k += 2) {
			let x = k === -d || (k !== d && v[max + k - 1] < v[max + k + 1]) ? v[max + k + 1] : v[max + k - 1] + 1;
			let y = x - k;
			while (x < n && y < m && a[x] === b[y]) {
				x++;
				y++;
			}
			v[max + k] = x;
			if (x >= n && y >= m) {
				return backtrack(trace, a, b, max);
			}
		}
	}
	return [];
}

function backtrack(trace, a, b, max) {
	const out = [];
	let x = a.length;
	let y = b.length;
	for (let d = trace.length - 1; d >= 0; d--) {
		const v = trace[d];
		const k = x - y;
		const prevK = k === -d || (k !== d && v[max + k - 1] < v[max + k + 1]) ? k + 1 : k - 1;
		const prevX = v[max + prevK];
		const prevY = prevX - prevK;
		while (x > prevX && y > prevY) {
			out.push([' ', a[--x]]);
			y--;
		}
		if (d > 0) {
			if (x === prevX) {
				out.push(['+', b[--y]]);
			} else {
				out.push(['-', a[--x]]);
			}
		}
	}
	return out.reverse();
}

// Diff shows the lines that differ with three lines of context, the rest
// folded.
export function Diff({ before, after }) {
	const ops = diffLines(before.split('\n'), after.split('\n'));
	const near = ops.map(() => false);
	ops.forEach(([op], i) => {
		if (op !== ' ') {
			for (let j = Math.max(0, i - 3); j <= Math.min(ops.length - 1, i + 3); j++) {
				near[j] = true;
			}
		}
	});
	const rows = [];
	let folded = 0;
	ops.forEach(([op, line], i) => {
		if (!near[i]) {
			folded++;
			return;
		}
		if (folded > 0) {
			rows.push(html`<div class="gap">${folded} unchanged ${folded === 1 ? 'line' : 'lines'}</div>`);
			folded = 0;
		}
		rows.push(html`<div class=${op === '+' ? 'add' : op === '-' ? 'del' : 'same'}>${line}</div>`);
	});
	if (folded > 0 && rows.length > 0) {
		rows.push(html`<div class="gap">${folded} unchanged ${folded === 1 ? 'line' : 'lines'}</div>`);
	}
	return html`<div class="diff">${rows.length === 0 ? html`<div class="gap">No difference</div>` : rows}</div>`;
}

// DiffDialog shows what applying the draft would change in the running
// config; with onApply, it offers to apply it.
export function DiffDialog({ close, onApply, notice }) {
	const [texts, setTexts] = useState(null);
	const [error, setError] = useState(null);
	useEffect(() => {
		Promise.all([request('GET', '/amf/config', { yaml: true, query: { live: '' } }), request('GET', '/amf/config', { yaml: true })]).then(
			([live, draft]) => setTexts({ live: live.data, draft: draft.data, etag: draft.etag }),
			e => setError(errorText(e)),
		);
	}, []);
	return html`
		<${Dialog} title=${onApply ? 'Apply the draft' : 'The draft against the running config'} wide foot=${html`
			<button class="btn" onClick=${() => close()}>${onApply ? 'Cancel' : 'Close'}</button>
			${onApply && html`<button class="btn primary" disabled=${!texts || texts.live === texts.draft} onClick=${() => close(texts.etag)}>Apply</button>`}
		`}>
			${onApply && html`<p>Applying builds a gateway from the draft and puts it into service: new requests use it, and requests in flight finish on the one they started on. The files don't change until the running config is saved.</p>`}
			${notice && html`<${Banner} kind="warn">${notice}<//>`}
			${error && html`<${Banner}>${error}<//>`}
			${texts ? html`<${Diff} before=${texts.live} after=${texts.draft} />` : !error && html`<p>Reading both copies...</p>`}
		<//>
	`;
}
