// The line diff the Apply dialog shows. It imports nothing, so its tests run
// without a browser.

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

