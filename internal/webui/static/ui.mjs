// What every view of the UI uses: the html template tag, icons, dialogs and
// toasts.

import { h } from 'preact';
import { useEffect, useRef, useState } from './vendor/hooks.mjs';
import htm from './vendor/htm.mjs';

export const html = htm.bind(h);

const paths = {
	chevron: 'M6 4l4 4-4 4',
	grip: 'M6 4h.01M10 4h.01M6 8h.01M10 8h.01M6 12h.01M10 12h.01',
	up: 'M8 12V4M4.5 7.5L8 4l3.5 3.5',
	down: 'M8 4v8M4.5 8.5L8 12l3.5-3.5',
	x: 'M4.5 4.5l7 7M11.5 4.5l-7 7',
	plus: 'M8 3.5v9M3.5 8h9',
	trash: 'M3.5 4.5h9M6.5 4.5V3h3v1.5M5 4.5l.6 8h4.8l.6-8',
	diff: 'M5 3v6M2 6h6M10 10h4M3 13h10',
	log: 'M3 4h10M3 8h10M3 12h6',
	reset: 'M3.5 8a4.5 4.5 0 1 0 1.3-3.2M3.5 3v2.5H6',
	copy: 'M5.5 5.5V3h7.5v7.5h-2.5M3 5.5h7.5V13H3z',
	link: 'M7 9a2.5 2.5 0 0 0 3.5 0l2-2a2.5 2.5 0 0 0-3.5-3.5l-.5.5M9 7a2.5 2.5 0 0 0-3.5 0l-2 2A2.5 2.5 0 0 0 7 12.5l.5-.5',
	full: 'M3 6V3h3M10 3h3v3M13 10v3h-3M6 13H3v-3',
	dock: 'M3 3h10v10H3zM3 9h10',
	key: 'M9.5 6.5a3 3 0 1 1-1 2.2L3 14H1.5v-1.5l1-1h1.5v-1.5h1.5l1-1',
	menu: 'M2.5 4h11M2.5 8h11M2.5 12h11',
	refresh: 'M13 8a5 5 0 1 1-1.46-3.54M13 2.5v3h-3',
	eye: 'M1.5 8S4 3.5 8 3.5 14.5 8 14.5 8 12 12.5 8 12.5 1.5 8 1.5 8zM8 10a2 2 0 1 0 0-4 2 2 0 0 0 0 4z',
	eyeOff: 'M1.5 8S4 3.5 8 3.5 14.5 8 14.5 8 12 12.5 8 12.5 1.5 8 1.5 8zM8 10a2 2 0 1 0 0-4 2 2 0 0 0 0 4zM2.5 2.5l11 11',
	collapse: 'M5 2.5l3 3 3-3M5 13.5l3-3 3 3',
	expand: 'M5 5.5l3-3 3 3M5 10.5l3 3 3-3',
};

// narrowScreen is the width below which the tree is a drawer and the log
// takes the entire screen; app.css uses the same width.
const narrowScreen = matchMedia('(max-width: 760px)');

// useNarrow reports whether the screen is narrow, again as it changes.
export function useNarrow() {
	const [narrow, setNarrow] = useState(narrowScreen.matches);
	useEffect(() => {
		const f = () => setNarrow(narrowScreen.matches);
		narrowScreen.addEventListener('change', f);
		return () => narrowScreen.removeEventListener('change', f);
	}, []);
	return narrow;
}

export function Icon({ name, size = 16, weight = 1.5 }) {
	return html`<svg width=${size} height=${size} viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width=${weight} stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d=${paths[name]} /></svg>`;
}

// The dialog on screen, if any: a function that renders it given close,
// which settles the promise openDialog returned with its value.
let showDialog = () => {};
let showToast = () => {};

export function openDialog(render) {
	return new Promise(resolve => {
		showDialog({ render, close: value => {
			showDialog(null);
			resolve(value);
		} });
	});
}

export function toast(message) {
	showToast(message);
}

// Dialogs renders the dialog and the toast, once, at the root of the page.
export function Dialogs() {
	const [dialog, setDialog] = useState(null);
	const [message, setMessage] = useState(null);
	const timer = useRef();
	showDialog = setDialog;
	showToast = m => {
		setMessage(m);
		clearTimeout(timer.current);
		timer.current = setTimeout(() => setMessage(null), 5000);
	};
	useEffect(() => {
		if (!dialog) {
			return;
		}
		const onKey = e => {
			if (e.key === 'Escape') {
				dialog.close(undefined);
			}
		};
		addEventListener('keydown', onKey);
		return () => removeEventListener('keydown', onKey);
	}, [dialog]);
	return html`
		${dialog && html`<div class="overlay" onMouseDown=${e => e.target === e.currentTarget && dialog.close(undefined)}>${dialog.render(dialog.close)}</div>`}
		${message && html`<div class="toast" role="status">${message}</div>`}
	`;
}

// Dialog is the frame of a dialog: a title, a body and its buttons.
export function Dialog({ title, wide, children, foot }) {
	return html`
		<div class=${'dialog' + (wide ? ' wide' : '')} role="dialog" aria-modal="true" aria-label=${title}>
			<h2>${title}</h2>
			<div class="dialog-body">${children}</div>
			<div class="dialog-foot">${foot}</div>
		</div>
	`;
}

// confirm asks a yes or no question; it resolves true for yes.
export function confirm({ title, body, yes = 'OK', danger = false }) {
	return openDialog(close => html`
		<${Dialog} title=${title} foot=${html`
			<button class="btn" onClick=${() => close(false)}>Cancel</button>
			<button class=${'btn ' + (danger ? 'danger' : 'primary')} onClick=${() => close(true)} ref=${focusOnMount}>${yes}</button>
		`}>${typeof body === 'string' ? html`<p>${body}</p>` : body}<//>
	`);
}

export function focusOnMount(el) {
	if (el && !el.dataset.focused) {
		el.dataset.focused = '1';
		setTimeout(() => el.focus(), 0);
	}
}

// Banner shows an error, a warning or a note above a view.
export function Banner({ kind = 'bad', children, onClose }) {
	return html`
		<div class=${'banner ' + kind} role=${kind === 'bad' ? 'alert' : 'status'}>
			<div class="grow">${children}</div>
			${onClose && html`<button class="btn ghost icon" title="Dismiss" onClick=${onClose}><${Icon} name="x" /></button>`}
		</div>
	`;
}

// Copy is a button that copies text to the clipboard.
export function Copy({ text, label = 'Copy' }) {
	const [done, setDone] = useState(false);
	return html`<button class="btn small" onClick=${async () => {
		if (await copyText(text)) {
			setDone(true);
			setTimeout(() => setDone(false), 1500);
		} else {
			toast('The browser refused the clipboard; select the text and copy it.');
		}
	}}><${Icon} name="copy" size=${14} />${done ? 'Copied' : label}</button>`;
}

// copyText puts text on the clipboard. The clipboard API is there only on
// an HTTPS page or localhost, and the admin UI is often served over plain
// HTTP, so it falls back to copying a selection, which works on both.
async function copyText(text) {
	if (window.isSecureContext && navigator.clipboard) {
		try {
			await navigator.clipboard.writeText(text);
			return true;
		} catch {
			// fall back
		}
	}
	const area = document.createElement('textarea');
	area.value = text;
	area.setAttribute('readonly', '');
	area.style.position = 'fixed';
	area.style.opacity = '0';
	document.body.append(area);
	area.select();
	let copied = false;
	try {
		copied = document.execCommand('copy');
	} catch {
		copied = false;
	}
	area.remove();
	return copied;
}

// Reveal is a key's field with a button over its right end that shows or
// hides what is in it; field makes the field, hidden or not. The button
// keeps the focus in the field, so a click doesn't count as leaving it.
export function Reveal({ field }) {
	const [shown, setShown] = useState(false);
	return html`
		<span class="reveal">
			${field(!shown)}
			<button
				type="button"
				class="reveal-toggle"
				title=${shown ? 'Hide' : 'Show'}
				aria-label=${shown ? 'Hide the key' : 'Show the key'}
				aria-pressed=${shown}
				onMouseDown=${e => e.preventDefault()}
				onClick=${() => setShown(!shown)}
			><${Icon} name=${shown ? 'eyeOff' : 'eye'} size=${15} /></button>
		</span>
	`;
}

// pathText writes a config path as the gateway's messages do: its keys
// joined with dots.
export function pathText(path) {
	return path.length === 0 ? 'the config' : path.join('.');
}

export function plural(n, one, many = one + 's') {
	return n + ' ' + (n === 1 ? one : many);
}
