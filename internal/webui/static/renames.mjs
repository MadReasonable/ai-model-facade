// The renames the page made, for the banner of names left behind. It
// imports nothing, so its tests run without a browser.

// The names of keys and groups this page renamed, old to new, by kind, so
// the places still naming an old name can be offered the new one. A name
// renamed again moves to its newest name, so typing a name a letter at a
// time records one rename, not one per letter.
const renames = new Map(); // kind + ':' + old -> new

export function recordRename(kind, old, to) {
	let chained = false;
	for (const [k, v] of renames) {
		if (k.startsWith(kind + ':') && v === old) {
			renames.set(k, to);
			chained = true;
		}
	}
	if (!chained) {
		renames.set(kind + ':' + old, to);
	}
	for (const [k, v] of renames) {
		if (k === kind + ':' + v) {
			renames.delete(k); // renamed back to what it was
		}
	}
}

export function renamedTo(kind, old) {
	return renames.get(kind + ':' + old);
}
