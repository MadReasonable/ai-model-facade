// CodeArea is a textarea for YAML or JSON with its text highlighted. A
// textarea can't color its own text, so its text is transparent, and a
// copy of it, colored, lies behind it at the same place, scrolled with it;
// typing, selection and undo stay the browser's own. The copy is built of
// elements, never HTML, since a config's text comes from its admins.

import { useRef } from './vendor/hooks.mjs';
import { html } from './ui.mjs';
import { jsonTokens, yamlTokens } from './highlight.mjs';

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
