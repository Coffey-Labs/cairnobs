// Laying out a discovered map (docs/maps.md, Tiers and layout): bands top
// to bottom, boxes in a centred grid in each band. A view holds at most a
// few hundred boxes, so this is plain arithmetic. Where a box sits is kept
// per view in this browser, so a new host slots in at the end instead of
// moving everything that was there.
import type { MapBox, MapView } from './api';

/** A box's centre and size; where it is in its band (row 0 is the band's
 * first), the top of the band's first row, and the width of its cell. */
export type Pos = { x: number; y: number; w: number; h: number; row?: number; top?: number; cell?: number };
export type Band = { label: string; y: number; h: number };
export type Layout = { pos: Record<string, Pos>; width: number; height: number; bands: Band[] };

export const WIDTH = 1000;
export const GAP = 18;
const PAD = 30, BAND_GAP = 40, LABEL = 26;

const SIZE: Record<string, [number, number]> = {
	internet: [170, 46], site: [228, 82], cluster: [228, 82], group: [210, 74], host: [188, 66], elsewhere: [188, 56], service: [200, 66]
};
const sizeOf = (b: MapBox) => SIZE[b.kind === 'elsewhere:service' ? 'service' : b.kind.startsWith('elsewhere') ? 'elsewhere' : b.kind] ?? SIZE.host;

const TIER_WORD: Record<string, string> = { edge: 'Edge', apps: 'Apps', data: 'Data' };

/** Which band a box goes in, and the band's label. On the service map
 * (tiers), a band is a layer: everything above needs what's below. */
function bandOf(v: MapView, b: MapBox, tiers: boolean, linked: Set<string>, needed: Set<string>): [number, string] {
	if (b.kind === 'internet') return [0, ''];
	// On the service map, what's outside this view and only needs what's in
	// it goes above it; what's needed goes below.
	if (b.kind.startsWith('elsewhere') && tiers && !needed.has(b.id)) return [5, 'Elsewhere, needing these'];
	if (b.kind.startsWith('elsewhere')) return [tiers ? 10_000 : 3, 'Elsewhere'];
	// A service nothing here links to would sit on the top layer with the
	// way in; it goes to the bottom instead.
	if (tiers && v.lines.length > 0 && !linked.has(b.id)) return [9_000, 'On their own'];
	if (tiers) return [10 + b.layer, ''];
	if (v.level === 'host') return b.id === v.focus ? [1, ''] : [2, 'Linked to'];
	if (v.level === 'overview' && b.kind === 'host' && b.via) return [2, 'Off site · own links'];
	if (v.level === 'overview' && b.kind === 'cluster') return [2, 'Clusters'];
	return [1, ''];
}

/** keptOrder puts ids in the order this browser last saw them, new ones after. */
export function keptOrder(key: string, ids: string[]): string[] {
	let old: string[] = [];
	try {
		old = JSON.parse(localStorage.getItem(key) ?? '[]');
	} catch {
		/* private window, or nothing kept */
	}
	const have = new Set(ids);
	const order = old.filter((id) => have.has(id));
	const seen = new Set(order);
	for (const id of ids) if (!seen.has(id)) order.push(id);
	try {
		localStorage.setItem(key, JSON.stringify(order.slice(0, 2000)));
	} catch {
		/* nowhere to keep it */
	}
	return order;
}

/** layout places v's boxes across width (the canvas's own, so text stays its real size). */
export function layout(v: MapView, order: string[], width = WIDTH, tiers = false): Layout {
	const WIDTH_ = Math.max(560, Math.round(width));
	const rank = new Map(order.map((id, i) => [id, i]));
	const bands = new Map<number, { label: string; boxes: MapBox[] }>();
	const linked = new Set(v.lines.flatMap((l) => [l.a, l.b]));
	const needed = new Set(v.lines.map((l) => l.b));
	for (const b of v.boxes) {
		const [n, label] = bandOf(v, b, tiers, linked, needed);
		if (!bands.has(n)) bands.set(n, { label, boxes: [] });
		bands.get(n)!.boxes.push(b);
	}
	if (tiers) {
		// Name each layer by the tiers in it: Edge, Apps · Data, …
		for (const [n, band] of bands) {
			if (n < 10 || n >= 9_000) continue;
			const ts = [...new Set(band.boxes.map((b) => b.tier ?? '').filter((t) => t))];
			band.label = ts.map((t) => TIER_WORD[t] ?? t).join(' · ');
		}
	}
	const pos: Record<string, Pos> = {};
	const out: Band[] = [];
	let y = PAD;
	for (const n of [...bands.keys()].sort((a, b) => a - b)) {
		const band = bands.get(n)!;
		band.boxes.sort((a, b) => (rank.get(a.id) ?? 1e9) - (rank.get(b.id) ?? 1e9));
		const top = y;
		if (band.label) y += LABEL;
		// Rows of the widest box's size, centred.
		const w = Math.max(...band.boxes.map((b) => sizeOf(b)[0]));
		const h = Math.max(...band.boxes.map((b) => sizeOf(b)[1]));
		const cols = Math.max(1, Math.min(band.boxes.length, Math.floor((WIDTH_ - 2 * PAD + GAP) / (w + GAP))));
		band.boxes.forEach((b, i) => {
			const row = Math.floor(i / cols), col = i % cols;
			// A band of one row is centred; in a band of several, a short last
			// row keeps the columns, so the gaps between them line up for the
			// lines that run down them.
			const inRow = band.boxes.length > cols ? cols : band.boxes.length;
			const left = (WIDTH_ - (inRow * w + (inRow - 1) * GAP)) / 2;
			const [bw, bh] = sizeOf(b);
			pos[b.id] = { x: left + col * (w + GAP) + w / 2, y: y + row * (h + GAP) + h / 2, w: bw, h: bh, row, top: y, cell: w };
		});
		y += Math.ceil(band.boxes.length / cols) * (h + GAP) - GAP;
		if (band.label) out.push({ label: band.label, y: top, h: y - top });
		y += BAND_GAP;
	}
	return { pos, width: WIDTH_, height: Math.max(y - BAND_GAP + PAD, 200), bands: out };
}

/** Where a line leaves box a heading for box b. */
export function edgePoint(a: Pos, b: Pos) {
	const dx = b.x - a.x, dy = b.y - a.y;
	const s = Math.min(a.w / 2 / Math.abs(dx || 1e-6), a.h / 2 / Math.abs(dy || 1e-6));
	return { x: a.x + dx * s, y: a.y + dy * s };
}

/** untangle orders boxes to cut crossings between layers: each box sits
 * near the average place of what it's linked to in the layer above (the
 * barycentre method), then a pass back up. It's the order a view starts
 * with; keptOrder keeps it from then on. */
export function untangle(v: MapView): string[] {
	const layers = new Map<number, string[]>();
	const needed = new Set(v.lines.map((l) => l.b));
	for (const b of [...v.boxes].sort((a, b) => a.name.localeCompare(b.name))) {
		const n = b.kind.startsWith('elsewhere') ? (needed.has(b.id) ? 10_000 : -1) : b.layer;
		if (!layers.has(n)) layers.set(n, []);
		layers.get(n)!.push(b.id);
	}
	const keys = [...layers.keys()].sort((a, b) => a - b);
	const near = new Map<string, string[]>();
	for (const l of v.lines) {
		near.set(l.a, [...(near.get(l.a) ?? []), l.b]);
		near.set(l.b, [...(near.get(l.b) ?? []), l.a]);
	}
	const place = new Map<string, number>();
	const settle = (ids: string[]) => ids.forEach((id, i) => place.set(id, (i + 0.5) / ids.length));
	const sweep = (order: number[]) => {
		order.forEach((k, i) => {
			const ids = layers.get(k)!;
			if (i > 0) {
				const key = (id: string, at: number) => {
					const xs = (near.get(id) ?? []).map((n) => place.get(n)).filter((x): x is number => x !== undefined);
					return xs.length ? xs.reduce((a, b) => a + b, 0) / xs.length : at;
				};
				const ks = new Map(ids.map((id, j) => [id, key(id, (j + 0.5) / ids.length)]));
				ids.sort((a, b) => ks.get(a)! - ks.get(b)!);
			}
			settle(ids);
		});
	};
	sweep(keys);
	place.clear();
	sweep([...keys].reverse());
	return keys.flatMap((k) => layers.get(k)!);
}
