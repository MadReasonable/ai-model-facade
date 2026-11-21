// Forms built from GET /amf/schema: a field per key, its control chosen by
// the key's schema, and the sheet that lays them out. A sheet edits a
// working copy of one node of the config and sends all of it back as JSON
// on each change, with the ETag it was read with; the gateway keeps every
// part sent unchanged as the file holds it, aliases and comments included,
// so only what was changed changes.

import { useEffect, useRef, useState } from './vendor/hooks.mjs';
import { clone, configURL, deleteKey, entriesOf, isObject, keysOf, parseOrdered, prettyOrdered, request, setKey } from './api.mjs';
import { edit, errorText, get, holdUnsent, refNames, renameNode, select, sharing, state, track, useStore } from './store.mjs';
import { Banner, html, Icon, pathText, Reveal, toast } from './ui.mjs';
import { CodeArea } from './code.mjs';

// useEntry reads the node at path in the draft with its ETag, again each
// time the draft changes, and commit sends a new value for it. While a
// commit the gateway refused is on screen, the value shown is the one sent,
// so it can be corrected.
export function useEntry(path) {
	const s = useStore();
	const key = path.join('\u0000');
	const [entry, setEntry] = useState({ loading: true });
	const [working, setWorking] = useState(null);
	const [error, setError] = useState(null);
	const [notes, setNotes] = useState([]);
	// Commits go out one at a time, each with the ETag the one before it
	// got back, which may be newer than the entry this render read; two at
	// once would send the same ETag, and the second would be refused.
	const known = useRef({ etag: null, missing: false });
	const queue = useRef(Promise.resolve());
	const read = next => {
		known.current = { etag: next.etag ?? null, missing: !!next.missing };
		setEntry(next);
	};
	useEffect(() => {
		if (get(path) === undefined) {
			read({ value: undefined, etag: null, missing: true }); // the draft just read has none
			return;
		}
		let current = true;
		request('GET', configURL(path)).then(
			r => current && read({ value: r.data, etag: r.etag }),
			e => current && (e.status === 404 ? read({ value: undefined, etag: null, missing: true }) : setEntry({ loadError: errorText(e) })),
		);
		return () => {
			current = false;
		};
	}, [key, s.draftETag]);
	useEffect(() => {
		setWorking(null);
		setError(null);
		setNotes([]);
	}, [key]);
	const send = async value => {
		try {
			const r = await edit(path, value, known.current.missing ? null : known.current.etag);
			read({ value, etag: r.etag });
			// A later commit may already show a newer value; it clears it.
			setWorking(w => (w === value ? null : w));
			setNotes(r.notes);
			return true;
		} catch (e) {
			setError(errorText(e));
			return false;
		}
	};
	const commit = value => {
		if (entry.loading || entry.loadError) {
			return Promise.resolve(false); // a value built from nothing read would replace what is there
		}
		setWorking(value);
		setError(null);
		// Tracked from now, not from when the edit before it is done, so an
		// action that settles the draft waits for this one too.
		const done = track(queue.current.then(() => send(value)));
		queue.current = done;
		return done;
	};
	return {
		loading: entry.loading,
		loadError: entry.loadError,
		missing: entry.missing,
		etag: entry.etag,
		value: working ?? entry.value,
		error,
		notes,
		commit,
		discard: () => {
			setWorking(null);
			setError(null);
		},
		dismissNotes: () => setNotes([]),
	};
}

// waiting returns what stands in for a view until the nodes it edits are
// read, so no control can build a value from what isn't there yet; null
// once they are.
export function waiting(...entries) {
	const failed = entries.find(e => e.loadError);
	if (failed) {
		return html`<div class="sheet"><${Banner}>${failed.loadError}<//></div>`;
	}
	return entries.some(e => e.loading) ? html`<div class="sheet"><p class="muted">Reading the draft...</p></div>` : null;
}

// EntryBanners shows what the last commit of an entry came to: the
// gateway's refusal, with a way back to the value it holds, or its notes.
export function EntryBanners({ entry }) {
	return html`
		${entry.loadError && html`<${Banner}>${entry.loadError}<//>`}
		${entry.error && html`<${Banner}>
			<div>The gateway refused this change, so the draft still holds the value it had: ${entry.error}</div>
			<div style="margin-top:8px"><button class="btn small" onClick=${entry.discard}>Show the draft's value</button></div>
		<//>`}
		${entry.notes.length > 0 && html`<${Banner} kind="warn" onClose=${entry.dismissNotes}>${entry.notes.join('\n')}<//>`}
	`;
}

// Sheet lays out a field for each key schema lists, except those in omit,
// for value, an object at path in the config, and one for each key value
// has that schema doesn't list, unless the sheet shows only part of its
// keys (only). onChange gets the new object.
export function Sheet({ schema, value, path, onChange, readOnly, omit = [], only }) {
	const v = isObject(value) ? value : {};
	const props = only ? Object.fromEntries(only.map(k => [k, schema.properties[k]])) : schema.properties ?? {};
	const known = new Set(Object.keys(schema.properties ?? {}));
	const extra = only ? [] : keysOf(v).filter(k => !known.has(k) && !omit.includes(k));
	return html`
		<div class="fields">
			${Object.entries(props).filter(([name]) => !omit.includes(name)).map(([name, ps]) => html`
				<${Field}
					key=${name}
					name=${name}
					schema=${ps}
					required=${(schema.required ?? []).includes(name)}
					value=${v[name]}
					path=${[...path, name]}
					readOnly=${readOnly}
					onChange=${x => onChange(withKey(v, name, x))}
				/>
			`)}
			${extra.map(name => html`
				<${Field}
					key=${name}
					name=${name}
					schema=${{ description: 'A key the schema doesn\'t list.' }}
					value=${v[name]}
					path=${[...path, name]}
					readOnly=${readOnly}
					onChange=${x => onChange(withKey(v, name, x))}
				/>
			`)}
		</div>
	`;
}

// withKey returns a copy of obj with k set to v, or removed for undefined,
// in its place.
export function withKey(obj, k, v) {
	const out = clone(obj ?? {});
	if (v === undefined) {
		deleteKey(out, k);
	} else {
		setKey(out, k, v);
	}
	return out;
}

// Field is one key: its name, its control, and what the schema says of it.
export function Field({ name, schema, required, value, path, readOnly, onChange }) {
	const shared = sharing(path);
	const [ownCopy, setOwnCopy] = useState(false);
	const viaAlias = shared && shared.via === 'alias' && !ownCopy;
	const inherited = shared && shared.via === 'merge' && shared.exact;
	const locked = readOnly || viaAlias;
	const set = value !== undefined;
	const remove = set && !locked && !inherited && !required && html`<button class="btn ghost icon" title=${schema.default !== undefined ? 'Remove; the default applies' : 'Remove'} onClick=${() => onChange(undefined)}><${Icon} name="reset" /></button>`;
	// A value whose parts each have a control that removes them gets the
	// button that removes it whole in its heading, apart from those.
	const inHeading = hasRemovableParts(schema);
	return html`
		<div class=${'field' + (shared ? ' shared' : '')}>
			<div class=${'name' + (set && !inherited ? ' set' : '') + (inHeading && remove ? ' with-tools' : '')}>
				<span>${name}${required && html`<span class="req" title="Required">*</span>`}</span>
				${inHeading && remove}
			</div>
			<div class="control">
				${viaAlias && html`<${SharedNote} shared=${shared} onCopy=${!readOnly && shared.exact ? () => setOwnCopy(true) : null} />`}
				${inherited && html`<div class="shared-note"><span class="badge shared">${'<< ' + anchorLabel(shared.anchor)}</span>A merge brings this value. Setting it here overrides the merge for this entry only.</div>`}
				<div class="row">
					<${Control} schema=${schema} value=${value} path=${path} readOnly=${locked} onChange=${onChange} />
					${!inHeading && remove}
				</div>
				${(schema.description || schema.default !== undefined) && html`<div class="help">${schema.description}${schema.default !== undefined && html` <span class="muted">Default: <code>${JSON.stringify(schema.default)}</code>.</span>`}</div>`}
			</div>
		</div>
	`;
}

export function anchorLabel(anchor) {
	return anchor.name ? '&' + anchor.name : pathText(anchor.path);
}

// SharedNote says a value is an anchor's, read through an alias, which an
// edit here would stop sharing; onCopy, if given, gives it a copy of its
// own to edit.
export function SharedNote({ shared, onCopy }) {
	const others = shared.anchor.users.length - 1;
	return html`
		<div class="shared-note">
			<span class="badge shared">${'*' + (shared.anchor.name || '')}</span>
			${shared.exact ? 'This value is' : 'This value is part of'} the anchor ${anchorLabel(shared.anchor)}, defined at <code>${pathText(shared.anchor.path)}</code>${others > 0 ? ', and ' + others + (others === 1 ? ' other value shares' : ' other values share') + ' it' : ''}.
			<button class="btn small" onClick=${() => select(state.selection, 'vars')}>Edit in &vars</button>
			${onCopy && html`<button class="btn small" onClick=${onCopy}>Give it its own value</button>`}
		</div>
	`;
}

// isKeyValues reports whether a value of schema is edited as a map of
// names to strings (KeyValues).
function isKeyValues(schema) {
	return schema.type === 'object' && !schema.properties && (schema.additionalProperties ?? {}).type === 'string';
}

// hasRemovableParts reports whether Control edits a value of schema in parts
// that each have a control of their own to remove them: a map of names to
// strings (KeyValues), a block of keys (Sheet), a list of maps (ObjectList),
// or a list of names (Chips).
function hasRemovableParts(schema) {
	if (schema.type === 'object') {
		return !!schema.properties || isKeyValues(schema);
	}
	if (schema.type === 'array') {
		const items = schema.items ?? {};
		return items.type === 'object' || (items.type === 'string' && items['x-ref'] !== 'protocols' && !items.enum);
	}
	return false;
}

// Control is the input for a value of schema.
function Control({ schema, value, path, readOnly, onChange }) {
	if (schema.type === undefined) {
		return html`<${JSONInput} value=${value} readOnly=${readOnly} onChange=${onChange} kind=${schema['x-kind']} />`;
	}
	if (schema.type === 'boolean') {
		const on = value ?? schema.default ?? false;
		return html`<label class="switch"><input type="checkbox" checked=${on} disabled=${readOnly} onChange=${e => onChange(e.target.checked)} /><span></span></label>`;
	}
	if (schema.type === 'string') {
		if (schema['x-secret']) {
			return html`<${SecretInput} value=${value} readOnly=${readOnly} onChange=${onChange} />`;
		}
		const choices = schema.enum ?? (schema['x-ref'] ? refNames(schema['x-ref']) : null);
		if (choices) {
			return html`<${Choice} value=${value} choices=${choices} fallback=${schema.default} readOnly=${readOnly} onChange=${onChange} />`;
		}
		return html`<${TextInput} value=${value} placeholder=${schema.default} readOnly=${readOnly} onCommit=${onChange} />`;
	}
	if (schema.type === 'integer' || schema.type === 'number') {
		return html`
			<${TextInput}
				type="number"
				value=${value}
				placeholder=${schema.default}
				min=${schema.minimum}
				step=${schema.type === 'integer' ? 1 : 'any'}
				readOnly=${readOnly}
				onCommit=${x => onChange(x === undefined ? undefined : schema.type === 'integer' ? parseInt(x, 10) : Number(x))}
			/>
			${schema['x-unit'] && html`<span class="unit">${schema['x-unit']}</span>`}
		`;
	}
	if (schema.type === 'array') {
		const items = schema.items ?? {};
		if (items.type === 'string') {
			if (items['x-ref'] === 'protocols' || items.enum) {
				return html`<${Checks} values=${value} choices=${items.enum ?? refNames('protocols')} readOnly=${readOnly} onChange=${onChange} />`;
			}
			return html`<${Chips} values=${value ?? []} known=${items['x-ref'] ? refNames(items['x-ref']) : null} readOnly=${readOnly} onChange=${x => onChange(x.length === 0 && value === undefined ? undefined : x)} />`;
		}
		if (items.type === 'object') {
			return html`<${ObjectList} schema=${items} value=${value ?? []} path=${path} readOnly=${readOnly} onChange=${onChange} />`;
		}
		return html`<${JSONInput} value=${value} readOnly=${readOnly} onChange=${onChange} />`;
	}
	if (schema.type === 'object') {
		if (schema.properties) {
			return html`<div class="nested" style="flex:1"><${Sheet} schema=${schema} value=${value} path=${path} readOnly=${readOnly} onChange=${x => onChange(keysOf(x).length === 0 ? undefined : x)} /></div>`;
		}
		const values = schema.additionalProperties ?? {};
		if (isKeyValues(schema)) {
			return html`<${KeyValues} value=${value} path=${path} secret=${values['x-secret']} known=${schema['x-ref'] ? refNames(schema['x-ref']) : null} readOnly=${readOnly} onChange=${onChange} />`;
		}
		return html`<${JSONInput} value=${value} readOnly=${readOnly} onChange=${onChange} />`;
	}
	return html`<${JSONInput} value=${value} readOnly=${readOnly} onChange=${onChange} />`;
}

// useListID returns an ID for a suggestion list that stays the same for the
// component's life, so the input it belongs to keeps finding it.
function useListID(prefix) {
	const id = useRef(null);
	if (id.current === null) {
		id.current = prefix + '-' + Math.random().toString(36).slice(2);
	}
	return id.current;
}

// sendPause is how long a field waits after typing stops before it sends
// its text to the draft.
const sendPause = 500;

// useLiveText is the text of a field for value, shown as text, which sends
// it with send: after a pause in typing if live, on Enter, when the field
// loses focus, and when an action settles the draft (holdUnsent). While
// the field has focus, the text stays as typed whatever the config says,
// so a reply to an earlier send can't undo later typing; it follows the
// config again once the field loses focus.
function useLiveText(shown, send, live) {
	const [text, setText] = useState(shown);
	const field = useRef(null);
	const latest = useRef({ text: shown, shown, send });
	latest.current = { ...latest.current, shown, send };
	const timer = useRef(null);
	const release = useRef(null);
	useEffect(() => {
		if (document.activeElement !== field.current) {
			setText(shown);
			latest.current.text = shown;
		}
	}, [shown]);
	const flush = () => {
		clearTimeout(timer.current);
		timer.current = null;
		release.current?.();
		release.current = null;
		const { text: t, shown: s, send: f } = latest.current;
		if (t !== s) {
			f(t);
		}
	};
	useEffect(() => () => {
		clearTimeout(timer.current);
		release.current?.();
	}, []);
	const onInput = e => {
		const t = e.currentTarget.value;
		latest.current.text = t;
		setText(t);
		if (!release.current) {
			release.current = holdUnsent(flush);
		}
		if (live) {
			clearTimeout(timer.current);
			timer.current = setTimeout(flush, sendPause);
		}
	};
	const reset = () => {
		clearTimeout(timer.current);
		timer.current = null;
		release.current?.();
		release.current = null;
		latest.current.text = latest.current.shown;
		setText(latest.current.shown);
	};
	return { text, field, onInput, flush, reset };
}

// TextInput is a field for a value, sent as it is typed (see
// useLiveText), or undefined for an empty field. A secret, such as a key,
// is sent only on Enter or when the field loses focus, since part of one
// would replace the one there; so is a field with live off, such as a
// client's name, whose change a dialog may first ask about.
export function TextInput({ value, placeholder, readOnly, onCommit, type = 'text', min, step, autofocus, secret, live = !secret }) {
	const shown = value === undefined || value === null ? '' : String(value);
	const { text, field: input, onInput, flush, reset } = useLiveText(shown, v => onCommit(v === '' ? undefined : v), live);
	const field = hidden => html`<input
		type=${type}
		class=${hidden ? 'secret' : undefined}
		autocomplete=${secret ? 'off' : undefined}
		value=${text}
		placeholder=${placeholder === undefined ? '' : String(placeholder)}
		min=${min}
		step=${step}
		disabled=${readOnly}
		spellcheck=${false}
		ref=${el => {
			input.current = el;
			if (autofocus && el && !el.dataset.focused) {
				el.dataset.focused = '1';
				setTimeout(() => el.focus(), 0);
			}
		}}
		onInput=${onInput}
		onBlur=${flush}
		onKeyDown=${e => {
			if (e.key === 'Enter') {
				flush();
			} else if (e.key === 'Escape') {
				reset();
			}
		}}
	/>`;
	return secret ? html`<${Reveal} field=${field} />` : field(false);
}

function Choice({ value, choices, fallback, readOnly, onChange }) {
	const options = value !== undefined && !choices.includes(value) ? [...choices, value] : choices;
	return html`
		<select value=${value ?? ''} disabled=${readOnly} onChange=${e => onChange(e.target.value === '' ? undefined : e.target.value)}>
			<option value="">${fallback !== undefined ? '(default: ' + fallback + ')' : '(not set)'}</option>
			${options.map(c => html`<option value=${c}>${c}${choices.includes(c) ? '' : ' (not defined)'}</option>`)}
		</select>
	`;
}

// SecretInput is a field for a secret the gateway holds, which it shows as
// <redacted>, since it never sends one back: dots while one is set, as a
// key field's, and a key typed into it replaces that one, on Enter or when
// the field is left. An empty field keeps the one there.
function SecretInput({ value, readOnly, onChange }) {
	return html`<${TextInput} value=${undefined} placeholder=${value === undefined ? 'not set' : '••••••••••••'} secret readOnly=${readOnly} onCommit=${v => v && onChange(v)} />`;
}

// Chips edits a list of names; known, if given, are the names defined,
// offered as suggestions, and a name not among them is marked.
export function Chips({ values, known, readOnly, onChange }) {
	const [text, setText] = useState('');
	const listID = useListID('names');
	const add = (t = text.trim()) => {
		if (t && !values.includes(t)) {
			onChange([...values, t]);
		}
		setText('');
	};
	return html`
		<div class="chips">
			${values.map((v, i) => html`
				<span class=${'chip' + (known && !known.includes(v) ? ' unknown' : '')} title=${known && !known.includes(v) ? 'Not defined in the config' : ''}>
					${v}${!readOnly && html`<button title="Remove" onClick=${() => onChange(values.filter((_, j) => j !== i))}>×</button>`}
				</span>
			`)}
			${values.length === 0 && readOnly && html`<span class="muted">none</span>`}
			${!readOnly && html`
				<input
					type="text"
					list=${known ? listID : undefined}
					placeholder="add"
					value=${text}
					onInput=${e => {
						// A pick from the suggestions arrives as one replacement
						if (known && known.includes(e.target.value) && (e.inputType === undefined || e.inputType === 'insertReplacementText')) {
							add(e.target.value);
						} else {
							setText(e.target.value);
						}
					}}
					onKeyDown=${e => {
						if (e.key === 'Enter' || e.key === ',') {
							e.preventDefault();
							add();
						} else if (e.key === 'Backspace' && text === '' && values.length > 0) {
							onChange(values.slice(0, -1));
						}
					}}
					onBlur=${() => add()}
				/>
				${known && html`<datalist id=${listID}>${known.filter(k => !values.includes(k)).map(k => html`<option value=${k} />`)}</datalist>`}
			`}
		</div>
	`;
}

// Checks picks from a fixed list; left out, the list means all of them.
function Checks({ values, choices, readOnly, onChange }) {
	const all = values === undefined;
	const on = all ? choices : values;
	return html`
		<div class="control">
			<div class="checks">
				${choices.map(c => html`
					<label><input type="checkbox" checked=${on.includes(c)} disabled=${readOnly} onChange=${e => onChange(e.target.checked ? choices.filter(x => x === c || on.includes(x)) : on.filter(x => x !== c))} />${c}</label>
				`)}
			</div>
			${all && html`<span class="help muted">Left out, so all of them.</span>`}
		</div>
	`;
}

// KeyValues edits a map of names to strings, such as a keyMap. A secret's
// name is changed with a rename of its node, since its value comes back as
// <redacted>, which a PUT under a new name has nothing to keep.
function KeyValues({ value, path, secret, known, readOnly, onChange }) {
	const entries = isObject(value) ? entriesOf(value) : [];
	const [adding, setAdding] = useState({ name: '', value: '' });
	const replace = (i, name, v) => {
		const out = {};
		entries.forEach(([k, x], j) => {
			if (j === i) {
				if (name !== undefined) {
					setKey(out, name, v);
				}
			} else {
				setKey(out, k, x);
			}
		});
		onChange(keysOf(out).length === 0 ? undefined : out);
	};
	const listID = useListID('kv');
	return html`
		<div class="control kv-list" style="flex:1">
			${entries.map(([k, v], i) => html`
				<div class="kv">
					<${TextInput} value=${k} readOnly=${readOnly} onCommit=${n => {
						if (!n || n === k) {
							return;
						}
						if (entries.some(([x]) => x === n)) {
							toast(n + ' is already in the list.');
							return;
						}
						if (secret) {
							renameNode([...path, k], n).catch(e => toast(errorText(e)));
						} else {
							replace(i, n, v);
						}
					}} />
					${secret ? html`<${SecretInput} value=${v} readOnly=${readOnly} onChange=${x => replace(i, k, x)} />` : html`<${TextInput} value=${v} readOnly=${readOnly} onCommit=${x => replace(i, k, x ?? '')} />`}
					${!readOnly && html`<button class="btn ghost icon" title="Remove" onClick=${() => replace(i, undefined)}><${Icon} name="trash" /></button>`}
				</div>
			`)}
			${!readOnly && html`
				<div class="kv">
					<input type="text" placeholder="name" autocomplete="off" list=${known ? listID : undefined} value=${adding.name} onInput=${e => setAdding({ ...adding, name: e.target.value })} />
					${secret
						? html`<${Reveal} field=${hidden => html`<input type="text" class=${hidden ? 'secret' : undefined} placeholder="value" autocomplete="off" spellcheck=${false} value=${adding.value} onInput=${e => setAdding({ ...adding, value: e.target.value })} />`} />`
						: html`<input type="text" placeholder="value" autocomplete="off" spellcheck=${false} value=${adding.value} onInput=${e => setAdding({ ...adding, value: e.target.value })} />`}
					<button class="btn icon" title="Add" aria-label="Add" disabled=${!adding.name || !adding.value} onClick=${() => {
						const out = clone(isObject(value) ? value : {});
						setKey(out, adding.name, adding.value);
						onChange(out);
						setAdding({ name: '', value: '' });
					}}><${Icon} name="plus" weight=${2.25} /></button>
				</div>
				${known && html`<datalist id=${listID}>${known.map(k => html`<option value=${k} />`)}</datalist>`}
			`}
		</div>
	`;
}

// ObjectList edits a list of maps, each with a sheet of its own.
function ObjectList({ schema, value, path, readOnly, onChange }) {
	return html`
		<div class="control" style="flex:1">
			${value.map((item, i) => html`
				<div class="nested">
					<div class="row" style="justify-content:space-between"><span class="muted">${'#' + (i + 1)}</span>
						${!readOnly && html`<button class="btn ghost icon" title="Remove" onClick=${() => onChange(value.filter((_, j) => j !== i))}><${Icon} name="trash" /></button>`}
					</div>
					<${Sheet} schema=${schema} value=${item} path=${[...path, String(i)]} readOnly=${readOnly} onChange=${x => onChange(value.map((y, j) => (j === i ? x : y)))} />
				</div>
			`)}
			${!readOnly && html`<div><button class="btn small" onClick=${() => onChange([...value, {}])}><${Icon} name="plus" size=${14} />Add</button></div>`}
		</div>
	`;
}

// JSONInput edits a value of any shape as JSON, such as params, which the
// gateway applies as merge patches; it commits once the text parses.
export function JSONInput({ value, readOnly, onChange, kind }) {
	const shown = value === undefined ? '' : prettyOrdered(value);
	const [error, setError] = useState(null);
	// Sent as typed once it parses; text that doesn't parse waits, and
	// shows why only once the field loses focus, not at every keystroke.
	const parse = t => {
		if (t.trim() === '') {
			return { value: undefined };
		}
		try {
			return { value: parseOrdered(t) };
		} catch (e) {
			return { error: 'Not JSON: ' + e.message };
		}
	};
	const { text, field, onInput, flush } = useLiveText(shown, t => {
		const p = parse(t);
		if (!p.error) {
			setError(null);
			onChange(p.value);
		}
	}, true);
	useEffect(() => setError(null), [shown]);
	const lines = Math.min(18, Math.max(3, text.split('\n').length));
	return html`
		<div class="control" style="flex:1" ref=${el => (field.current = el?.querySelector('textarea') ?? null)}>
			<${CodeArea} lang="json" rows=${lines} disabled=${readOnly} placeholder=${kind === 'params' ? '{"all": {...}, "openAiChat": {...}}' : 'JSON'} value=${text} onInput=${onInput} onBlur=${() => {
				setError(parse(text).error ?? null);
				flush();
			}} />
			${error && html`<span class="help" style="color:var(--bad)">${error}</span>`}
		</div>
	`;
}
