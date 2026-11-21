// CodeArea is a textarea for YAML or JSON with its text highlighted. A
// textarea can't color its own text, so its text is transparent, and a
// copy of it, colored, lies behind it at the same place, scrolled with it;
// typing, selection and undo stay the browser's own. The copy is built of
// elements, never HTML, since a config's text comes from its admins.

import { useRef } from './vendor/hooks.mjs';
import { html } from './ui.mjs';

export function CodeArea({ lang = 'yaml', value, rows, ...props }) {
	const back = useRef();
	const sync = e => {
		if (back.current) {
			back.current.scrollTop = e.currentTarget.scrollTop;
			back.current.scrollLeft = e.currentTarget.scrollLeft;
		}
	};
	const lines = (value ?? '').split('\n');
	return html`
		<div class="code-area">
			<pre class="code-back" ref=${back} aria-hidden="true">${lines.map(line => html`${(lang === 'json' ? jsonTokens : yamlTokens)(line).map(([kind, text]) => (kind ? html`<span class=${'tok-' + kind}>${text}</span>` : text))}${'\n'}`)}${' '}</pre>
			<textarea class="code" rows=${rows} spellcheck=${false} value=${value} onScroll=${sync} ...${props}></textarea>
		</div>
	`;
}

// yamlTokens splits one line of YAML into [kind, text] pairs, kind '' for
// plain text. It reads a line at a time, so a block scalar's lines are
// colored as if they were YAML; for a config, that is rare enough.
export function yamlTokens(line) {
	const out = [];
	const comment = commentStart(line);
	const body = comment < 0 ? line : line.slice(0, comment);
	const key = /^(\s*(?:-\s+)*)((?:"(?:[^"\\]|\\.)*"|'(?:[^']|'')*'|[^\s#'"{[\]},][^#]*?))(\s*:)(?=\s|$)/.exec(body);
	let rest = body;
	if (key) {
		out.push(['', key[1]], [key[2] === '<<' ? 'anchor' : 'key', key[2]], ['', key[3]]);
		rest = body.slice(key[0].length);
	}
	const value = /"(?:[^"\\]|\\.)*"|'(?:[^']|'')*'|[&*][^\s,{}[\]]+|\$\{[A-Za-z_][A-Za-z0-9_]*\}|(?<![\w.:/-])(?:-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?|true|false|null|~)(?![\w.:/-])/g;
	let at = 0;
	for (const m of rest.matchAll(value)) {
		out.push(['', rest.slice(at, m.index)]);
		const t = m[0];
		out.push([t[0] === '"' || t[0] === "'" ? 'string' : t[0] === '&' || t[0] === '*' ? 'anchor' : t[0] === '$' ? 'env' : 'literal', t]);
		at = m.index + t.length;
	}
	out.push(['', rest.slice(at)]);
	if (comment >= 0) {
		out.push(['comment', line.slice(comment)]);
	}
	return out.filter(([, t]) => t !== '');
}

// commentStart returns where a line's comment begins: a # at the start or
// after a space, outside quotes; -1 if it has none.
function commentStart(line) {
	let quote = '';
	for (let i = 0; i < line.length; i++) {
		const c = line[i];
		if (quote) {
			if (c === '\\' && quote === '"') {
				i++;
			} else if (c === quote) {
				quote = '';
			}
		} else if (c === '"' || c === "'") {
			quote = c;
		} else if (c === '#' && (i === 0 || /\s/.test(line[i - 1]))) {
			return i;
		}
	}
	return -1;
}

// jsonTokens splits one line of JSON into [kind, text] pairs: a string
// followed by a colon is a key.
export function jsonTokens(line) {
	const out = [];
	const token = /("(?:[^"\\]|\\.)*")(\s*:)?|-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?|\btrue\b|\bfalse\b|\bnull\b/g;
	let at = 0;
	for (const m of line.matchAll(token)) {
		out.push(['', line.slice(at, m.index)]);
		if (m[1]) {
			out.push([m[2] ? 'key' : 'string', m[1]]);
			if (m[2]) {
				out.push(['', m[2]]);
			}
		} else {
			out.push(['literal', m[0]]);
		}
		at = m.index + m[0].length;
	}
	out.push(['', line.slice(at)]);
	return out.filter(([, t]) => t !== '');
}
