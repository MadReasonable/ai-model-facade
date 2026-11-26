// The log panel: the gateway's copy of its log, read from GET /amf/logs
// and followed as it grows. It is hidden, docked below the sheet at a
// height it can be dragged to, or the entire main pane.

import { useEffect, useRef, useState } from './vendor/hooks.mjs';
import { followLogs, keysOf, parseOrdered } from './api.mjs';
import { errorText } from './store.mjs';
import { html, Icon } from './ui.mjs';

// keep is how many lines the panel holds; past it, the oldest go.
const keep = 3000;

const levels = ['debug', 'info', 'warning', 'error'];

function remembered(name, fallback) {
	try {
		return localStorage.getItem('amf-log-' + name) ?? fallback;
	} catch {
		return fallback;
	}
}

function remember(name, value) {
	try {
		localStorage.setItem('amf-log-' + name, value);
	} catch {
		// storage off: the panel starts as it is by default next time
	}
}

// useLogLayout holds where the panel is and its height while docked, each
// remembered for the next visit; setHeight remembers the height unless
// keep is false, as during a drag, which keeps only the height it ends at.
export function useLogLayout() {
	const [mode, setMode] = useState(remembered('mode', 'hidden'));
	const [height, setHeight] = useState(Number(remembered('height', '280')));
	return {
		mode,
		height,
		setMode: m => {
			remember('mode', m);
			setMode(m);
		},
		setHeight: (h, keep = true) => {
			if (keep) {
				remember('height', String(h));
			}
			setHeight(h);
		},
	};
}

// timeOrder makes the times of one gateway's log, in RFC 3339 with a
// fraction Go trims of trailing zeros, compare as strings in time order.
function timeOrder(t) {
	return t.replace(/(?:\.(\d+))?(Z|[+-]\d\d:\d\d)$/, (_, frac = '', zone) => '.' + (frac + '000000000').slice(0, 9) + zone);
}

// useLogStream follows the log at level and above while on, starting with
// the last lines held, and, after the stream drops, again from the time of
// the last line it got, so a reconnect neither repeats nor loses lines the
// gateway still holds.
function useLogStream(on, level) {
	const [lines, setLines] = useState([]);
	const [note, setNote] = useState(null);
	const last = useRef(null);
	useEffect(() => {
		if (!on) {
			return;
		}
		let stopped = false;
		let stream = null;
		let retry = 1000;
		setLines([]);
		last.current = null;
		const start = () => {
			const query = { format: 'json', level };
			if (last.current) {
				query.since = last.current;
			} else {
				query.limit = '500';
			}
			stream = followLogs(query, ({ event, data }) => {
				retry = 1000;
				if (event === 'message') {
					let entry;
					try {
						entry = parseOrdered(data);
					} catch {
						return;
					}
					if (last.current && timeOrder(entry.time) <= timeOrder(last.current)) {
						return; // since takes the line logged at that instant again
					}
					last.current = entry.time;
					setLines(l => (l.length >= keep ? [...l.slice(l.length - keep + 1), entry] : [...l, entry]));
				} else if (event === 'dropped') {
					const n = JSON.parse(data).dropped;
					setLines(l => [...l, { note: n + (n === 1 ? ' line' : ' lines') + ' dropped here: the panel read them more slowly than the gateway logged them.' }]);
				} else if (event === 'end') {
					setNote(JSON.parse(data).reason);
				}
			});
			setNote(null);
			stream.done.then(
				() => !stopped && again('The stream closed.'),
				e => !stopped && again(errorText(e)),
			);
		};
		const again = why => {
			setNote(why + ' Reconnecting in ' + Math.round(retry / 1000) + ' s.');
			setTimeout(() => !stopped && start(), retry);
			retry = Math.min(retry * 2, 30000);
		};
		start();
		return () => {
			stopped = true;
			stream?.stop();
		};
	}, [on, level]);
	return { lines, note };
}

export function LogPanel({ mode, setMode, height, setHeight, narrow }) {
	const [level, setLevel] = useState(remembered('level', 'info'));
	const [filter, setFilter] = useState('');
	const [paused, setPaused] = useState(false);
	const { lines, note } = useLogStream(mode !== 'hidden', level);
	const [frozen, setFrozen] = useState(null);
	const box = useRef();
	const atBottom = useRef(true);
	const shown = paused ? frozen ?? lines : lines;
	const needle = filter.trim().toLowerCase();
	const visible = needle ? shown.filter(l => (l.note ?? JSON.stringify(l)).toLowerCase().includes(needle)) : shown;
	useEffect(() => {
		if (atBottom.current && box.current) {
			box.current.scrollTop = box.current.scrollHeight;
		}
	});
	if (mode === 'hidden') {
		return null;
	}
	const startResize = e => {
		e.preventDefault();
		const startY = e.clientY;
		const startHeight = height;
		const handle = e.currentTarget;
		handle.classList.add('active');
		const move = ev => setHeight(Math.max(120, Math.min(innerHeight - 160, startHeight + startY - ev.clientY)), false);
		const up = ev => {
			removeEventListener('pointermove', move);
			removeEventListener('pointerup', up);
			handle.classList.remove('active');
			setHeight(Math.max(120, Math.min(innerHeight - 160, startHeight + startY - ev.clientY)));
		};
		addEventListener('pointermove', move);
		addEventListener('pointerup', up);
	};
	return html`
		${mode === 'docked' && html`<div class="log-resize" onPointerDown=${startResize} title="Drag to resize"></div>`}
		<div class="log" style=${mode === 'docked' ? 'height:' + height + 'px' : ''}>
			<div class="log-bar">
				<span class="title">Log</span>
				<select value=${level} title="The lowest level shown" onChange=${e => {
					setLevel(e.target.value);
					remember('level', e.target.value);
				}}>${levels.map(l => html`<option value=${l}>${l} and above</option>`)}</select>
				<input type="search" placeholder="Filter" value=${filter} onInput=${e => setFilter(e.target.value)} />
				<button class=${'btn small' + (paused ? ' primary' : '')} onClick=${() => {
					setFrozen(paused ? null : lines);
					setPaused(!paused);
				}}>${paused ? 'Resume' : 'Pause'}</button>
				<span class="grow muted" style="font-size:12px">${note ?? (paused ? 'Paused; new lines are held.' : '')}</span>
				${!narrow && html`<div class="seg">
					<button class=${mode === 'docked' ? 'on' : ''} title="Below the sheet" onClick=${() => setMode('docked')}><${Icon} name="dock" size=${14} /></button>
					<button class=${mode === 'full' ? 'on' : ''} title="The entire pane" onClick=${() => setMode('full')}><${Icon} name="full" size=${14} /></button>
				</div>`}
				<button class="btn ghost icon" title="Hide the log" onClick=${() => setMode('hidden')}><${Icon} name="x" /></button>
			</div>
			<div class="log-lines" ref=${box} onScroll=${e => {
				const el = e.currentTarget;
				atBottom.current = el.scrollHeight - el.scrollTop - el.clientHeight < 24;
			}}>
				${visible.length === 0 && html`<div class="log-note">${lines.length === 0 ? 'No lines yet.' : 'No line matches the filter.'}</div>`}
				${visible.map(l => (l.note ? html`<div class="log-note">${l.note}</div>` : html`<${Line} entry=${l} />`))}
			</div>
		</div>
	`;
}

// Line shows one record: its time, level and message, then its attributes
// as key=value, groups as dotted keys.
function Line({ entry }) {
	const attrs = [];
	const walk = (prefix, obj) => {
		for (const k of keysOf(obj)) {
			if (prefix === '' && (k === 'time' || k === 'level' || k === 'msg')) {
				continue;
			}
			const v = obj[k];
			if (v !== null && typeof v === 'object' && !Array.isArray(v)) {
				walk(prefix + k + '.', v);
			} else {
				attrs.push([prefix + k, typeof v === 'string' ? v : JSON.stringify(v)]);
			}
		}
	};
	walk('', entry);
	const time = new Date(entry.time);
	return html`
		<div class=${'log-line ' + entry.level} title=${entry.time}>
			<span class="time">${time.toLocaleTimeString(undefined, { hour12: false }) + '.' + String(time.getMilliseconds()).padStart(3, '0')}</span>
			<span class="level">${entry.level}</span>
			<span>${entry.msg}${attrs.map(([k, v]) => html` <span class="attrs"><b>${k}=</b>${v}</span>`)}</span>
		</div>
	`;
}

