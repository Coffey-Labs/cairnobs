<script lang="ts">
	// The service map drawn by hand in topology.yaml. The discovered map
	// (the page) is the default; this stays beside it while topology.yaml
	// is set.
	import Panel from '$lib/Panel.svelte';
	import Status from '$lib/Status.svelte';
	import { paths as ICONS } from '$lib/icons';
	import { topology } from '$lib/topology.svelte';
	import type { ServiceNode, Severity } from '$lib/api';

	const t = topology();
	// Condensed: tiers wrap at six to a line, so it fits a laptop screen.
	const ROW_H = 118, NODE_W = 168, NODE_H = 54, GAP = 22, PAD = 24, LABEL_W = 96, PER_LINE = 6, LINE_H = NODE_H + 26;
	const TIER_NAME: Record<string, string> = { edge: 'Edge', public: 'Public services', apps: 'Mail & apps', data: 'Data layer', hosts: 'Hosts' };

	const services = $derived(t.data?.services ?? []);
	const byId = $derived(Object.fromEntries(services.map((s) => [s.id, s])) as Record<string, ServiceNode>);
	const tiers = $derived((t.data?.tiers ?? []).filter((tier) => services.some((s) => s.tier === tier)));

	// Rows are ordered bottom-up so each service sits above what it needs
	// (the average position of its needs), which keeps lines from crossing.
	const layout = $derived.by(() => {
		const order: Record<string, string[]> = {};
		const slot: Record<string, number> = {};
		for (const tier of [...tiers].reverse()) {
			const row = services.filter((s) => s.tier === tier);
			const score = (s: ServiceNode, i: number) => {
				const xs = s.needs.map((n) => slot[n]).filter((v) => v !== undefined);
				return xs.length ? xs.reduce((a, b) => a + b, 0) / xs.length : (i + 0.5) / row.length;
			};
			const sorted = tier === 'hosts' ? row : row.map((s, i) => ({ s, k: score(s, i) })).sort((a, b) => a.k - b.k).map((x) => x.s);
			order[tier] = sorted.map((s) => s.id);
			sorted.forEach((s, i) => (slot[s.id] = (i + 0.5) / sorted.length));
		}
		const cols = Math.min(PER_LINE, Math.max(1, ...tiers.map((tier) => order[tier].length)));
		const width = LABEL_W + PAD * 2 + cols * NODE_W + (cols - 1) * GAP;
		const pos: Record<string, { x: number; y: number }> = {};
		const tierY: Record<string, number> = {};
		let y = PAD + 14;
		for (const tier of tiers) {
			tierY[tier] = y;
			const ids = order[tier];
			for (let line = 0; line * PER_LINE < ids.length; line++) {
				const chunk = ids.slice(line * PER_LINE, (line + 1) * PER_LINE);
				const rowW = chunk.length * NODE_W + (chunk.length - 1) * GAP;
				const x0 = LABEL_W + PAD + (width - LABEL_W - PAD * 2 - rowW) / 2;
				chunk.forEach((id, i) => (pos[id] = { x: x0 + i * (NODE_W + GAP), y: y + line * LINE_H }));
			}
			y += Math.ceil(ids.length / PER_LINE) * LINE_H + (ROW_H - LINE_H);
		}
		return { pos, width, height: y - (ROW_H - LINE_H) - (LINE_H - NODE_H) + PAD, tierY };
	});
	const pos = $derived(layout.pos);
	const edges = $derived(services.flatMap((s) => s.needs.filter((n) => pos[n]).map((n) => ({ from: s.id, to: n }))));

	// What's shown: what the pointer is on, else what was clicked, else the
	// worst service, else PostgreSQL (the demo's example of a wide blast radius).
	let hover = $state<string | null>(null);
	let picked = $state<string | null>(null);
	const rank = (s: string) => ({ critical: 5, serious: 4, warning: 3, waiting: 1 })[s] ?? 0;
	const fallback = $derived.by(() => {
		const bad = services.filter((s) => s.tier !== 'hosts' && rank(s.status) >= 3).sort((a, b) => rank(b.status) - rank(a.status))[0];
		return bad?.id ?? (byId['patroni'] ? 'patroni' : services[0]?.id);
	});
	const focus = $derived(hover ?? (picked && byId[picked] ? picked : fallback));

	// "need": what it relies on, all the way down; "hot": everything that
	// breaks with it, all the way up.
	const walk = (id: string, next: (x: string) => string[]) => {
		const set = new Set<string>();
		const todo = [id];
		while (todo.length) for (const n of next(todo.pop()!)) if (!set.has(n)) { set.add(n); todo.push(n); }
		return set;
	};
	const needsOf = (id: string) => walk(id, (x) => byId[x]?.needs ?? []);
	const breaksOf = (id: string) => walk(id, (x) => services.filter((s) => s.needs.includes(x)).map((s) => s.id));
	const lit = $derived.by(() => {
		if (!focus) return null;
		const need = needsOf(focus), hot = breaksOf(focus);
		return { id: focus, need, hot, has: (x: string) => x === focus || need.has(x) || hot.has(x) };
	});
	const edgeKind = (e: { from: string; to: string }) => {
		if (!lit) return '';
		if ((e.from === lit.id || lit.need.has(e.from)) && lit.need.has(e.to)) return 'need';
		if ((e.to === lit.id || lit.hot.has(e.to)) && lit.hot.has(e.from)) return 'hot';
		return 'dim';
	};

	// A line into something in trouble takes that trouble's colour.
	const SEV: Record<string, string> = { warning: 'var(--warning)', serious: 'var(--serious)', critical: 'var(--critical)' };
	const edgeStroke = (to: string) => SEV[byId[to]?.status ?? ''] ?? 'var(--input)';
	const glyphColor = (st: string) => (st === 'good' ? 'var(--good)' : (SEV[st] ?? 'var(--muted-fg)'));
	const icon = (st: string) => ICONS[(st in ICONS ? st : 'off') as keyof typeof ICONS];

	function path(a: { x: number; y: number }, b: { x: number; y: number }) {
		if (a.y === b.y) { // neighbours in the same line: side to side
			const left = a.x < b.x;
			const x1 = left ? a.x + NODE_W : a.x, x2 = left ? b.x : b.x + NODE_W, y = a.y + NODE_H / 2;
			return `M${x1},${y} L${x2},${y}`;
		}
		const x1 = a.x + NODE_W / 2, y1 = a.y + NODE_H, x2 = b.x + NODE_W / 2, y2 = b.y;
		const dy = Math.max(30, (y2 - y1) / 2);
		return `M${x1},${y1} C${x1},${y1 + dy} ${x2},${y2 - dy} ${x2},${y2}`;
	}
	const short = (s: string, n = 21) => (s.length > n ? s.slice(0, n - 1) + '…' : s);
	function key(e: KeyboardEvent, s: ServiceNode) {
		if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); picked = s.id; }
	}

	const sel = $derived(focus ? byId[focus] : null);
	const directNeeds = $derived(sel ? sel.needs.map((n) => byId[n]).filter(Boolean) : []);
	const affected = $derived(sel ? [...breaksOf(sel.id)].map((n) => byId[n]).filter(Boolean) : []);
	// Quick picks: whatever is in trouble, then the services everything leans on.
	const chips = $derived.by(() => {
		const bad = services.filter((s) => rank(s.status) >= 3).map((s) => s.id);
		const keyOnes = ['patroni', 'inbuxa', 'cloudflare', 'nginx'].filter((id) => byId[id]);
		return [...new Set([...bad, ...keyOnes])].slice(0, 6);
	});
	const sev = (s: string) => s as Severity;
</script>

{#if chips.length}
	<div class="chips hand" role="group" aria-label="Jump to">
		{#each chips as id (id)}
			<button class="chip" class:on={focus === id} onclick={() => (picked = id)}>
				{#if rank(byId[id].status) >= 3}<i class="dot" style="background:{glyphColor(byId[id].status)}"></i>{/if}{byId[id].name}
			</button>
		{/each}
	</div>
{/if}

{#if t.error}<p class="foot-note" style="color:var(--critical)">{t.error}</p>{/if}

{#snippet li(s: ServiceNode)}
	<li>
		<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" style="color:{glyphColor(s.status)}">{@html icon(s.status)}</svg>
		<button class="linkish" onclick={() => (picked = s.id)}>{s.name}</button>
		<span class="muted">{s.status === 'good' ? s.sub : s.word}</span>
	</li>
{/snippet}

{#if t.data && !t.data.configured}
	<Panel title="Not set up"><p class="sub">Point <span class="mono">topology</span> in obs-beacond.yaml at a topology file. See docs/formats.md.</p></Panel>
{:else if t.data}
	<Panel flush>
		<div class="map-canvas">
			<div class="scroll">
				<svg viewBox="0 0 {layout.width} {layout.height}" style="min-width:{Math.min(layout.width, 720)}px; max-width:{layout.width}px" role="img" aria-label="Service map">
					{#each tiers as tier, r (tier)}
						<text class="region-l" x={PAD} y={layout.tierY[tier] - 10}>{TIER_NAME[tier] ?? tier}</text>
						{#if r > 0}<line class="rule" x1={PAD} x2={layout.width - PAD} y1={layout.tierY[tier] - 26} y2={layout.tierY[tier] - 26} />{/if}
					{/each}
					{#each edges as e (e.from + '>' + e.to)}
						{@const k = edgeKind(e)}
						<path class="edge {k}" d={path(pos[e.from], pos[e.to])} stroke={k === '' || k === 'dim' ? edgeStroke(e.to) : undefined} />
						{#if k === 'need' || k === 'hot'}<path class="edge {k} flow" d={path(pos[e.from], pos[e.to])} />{/if}
					{/each}
					{#each services as s (s.id)}
						{@const p = pos[s.id]}
						{#if p}
							<g class="node" class:dim={lit && !lit.has(s.id)} class:sel={focus === s.id} class:need={lit?.need.has(s.id)} class:hot={lit?.hot.has(s.id)}
								transform="translate({p.x},{p.y})" role="button" tabindex="0" aria-label="{s.name}: {s.word}"
								onmouseenter={() => (hover = s.id)} onmouseleave={() => (hover = null)} onfocus={() => (hover = s.id)} onblur={() => (hover = null)}
								onclick={() => (picked = s.id)} onkeydown={(e) => key(e, s)}>
								<rect class="box" width={NODE_W} height={NODE_H} rx="9" />
								<svg x="10" y="10" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" style="color:{glyphColor(s.status)}">{@html icon(s.status)}</svg>
								<text class="nm" x="30" y="22">{short(s.name, 19)}</text>
								<text class="sb" x="10" y="42" fill={s.status === 'good' || s.status === 'waiting' ? undefined : glyphColor(s.status)}>{short(s.status === 'good' || !s.word ? s.sub || s.word : s.word, 26)}</text>
							</g>
						{/if}
					{/each}
				</svg>
			</div>
			<div class="map-legend">
				<span><i class="ln" style="border-color:var(--serious)"></i>breaks with it</span>
				<span><i class="ln" style="border-color:var(--s3)"></i>what it needs</span>
				<span><i class="ln" style="border-color:var(--input)"></i>depends on (lines run downward)</span>
			</div>
		</div>

		{#if sel}
			<div class="map-panel" aria-live="polite">
				<div>
					<h2>{sel.name}</h2>
					{#if sel.sub}<div class="muted mt2">{sel.sub}</div>{/if}
					<div class="mt8"><Status s={sev(sel.status)} word={sel.word} /></div>
					{#if sel.cause}<p class="cause">Likely cause: <b>{sel.cause}</b>. Its alerts are grouped under that failure.</p>{/if}
					{#if sel.note}<p class="muted note">{sel.note}</p>{/if}
					{#if sel.signals.length}
						<h3>Made of</h3>
						<ul>
							{#each sel.signals as sg, i (i)}
								<li>
									<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" style="color:{glyphColor(sg.status)}">{@html icon(sg.status)}</svg>
									{#if sg.href}<a href={sg.href}>{sg.name}</a>{:else}<span>{sg.name}</span>{/if}
									<span class="muted">{sg.host ? sg.host + ' · ' : ''}{sg.word}</span>
								</li>
							{/each}
						</ul>
					{/if}
				</div>
				<div>
					<h3>Needs</h3>
					<ul>
						{#each directNeeds as n (n.id)}{@render li(n)}{:else}<li class="muted">Nothing we run</li>{/each}
					</ul>
				</div>
				<div>
					<h3>If this breaks · {affected.length} affected</h3>
					<ul>
						{#each affected as n (n.id)}{@render li(n)}{:else}<li class="muted">Nothing else depends on it</li>{/each}
					</ul>
				</div>
			</div>
		{/if}
	</Panel>
	<p class="foot-note">Drawn from topology.yaml; Cairn OBS fills in the state. A service's colour is the worst of its parts, and a line into something in trouble takes that trouble's colour.</p>
{:else}
	<p class="empty">Loading…</p>
{/if}

<style>
	.phead { flex-wrap: wrap; }
	.chips { display: flex; gap: 6px; flex-wrap: wrap; margin: 0 0 12px; }
	.chip { display: inline-flex; align-items: center; gap: 6px; height: 30px; padding: 0 12px; border-radius: 20px; border: 1px solid var(--border); background: var(--card); color: var(--muted-fg); font-size: 12px; cursor: pointer; }
	.chip.on { border-color: var(--primary); color: var(--foreground); background: var(--accent); }
	.chip .dot { width: 8px; height: 8px; border-radius: 50%; }
	.map-canvas {
		position: relative; border-bottom: 1px solid var(--border); padding: 8px 6px 52px;
		background-image: radial-gradient(color-mix(in oklab, var(--muted-fg) 22%, transparent) 1px, transparent 1px); background-size: 22px 22px;
	}
	.scroll { overflow-x: auto; }
	.scroll > svg { display: block; width: 100%; height: auto; margin: 0 auto; font-family: var(--sans); }
	.region-l { fill: var(--muted-fg); font: 600 10px var(--mono); letter-spacing: .16em; text-transform: uppercase; }
	.rule { stroke: var(--border); stroke-dasharray: 2 4; }
	.edge { fill: none; stroke-width: 1.5; opacity: .7; transition: opacity .15s; }
	.edge.need { stroke: var(--s3); stroke-width: 2.5; opacity: .5; }
	.edge.hot { stroke: var(--serious); stroke-width: 2.5; opacity: .5; }
	.edge.flow { opacity: 1; }
	.edge.dim { opacity: .12; }
	.node { cursor: pointer; outline: none; transition: opacity .15s; }
	.node .box { fill: var(--card); stroke: var(--input); stroke-width: 1.2; }
	.node:hover .box, .node:focus-visible .box { stroke: var(--muted-fg); }
	.node.sel .box { stroke: var(--primary); stroke-width: 2; }
	.node.need .box { stroke: var(--s3); stroke-width: 2; }
	.node.hot .box { stroke: var(--serious); stroke-width: 2; }
	.node.dim { opacity: .38; }
	.nm { fill: var(--foreground); font: 600 12.5px var(--sans); }
	.sb { fill: var(--muted-fg); font: 400 11px var(--sans); }
	.map-legend {
		position: absolute; left: 14px; bottom: 12px; display: flex; gap: 6px 14px; flex-wrap: wrap; font-size: 12px; color: var(--muted-fg);
		background: color-mix(in oklab, var(--card) 88%, transparent); border: 1px solid var(--border); padding: 6px 10px; border-radius: 6px;
	}
	.map-legend span { display: inline-flex; align-items: center; gap: 6px; }
	.map-legend i.ln { width: 18px; border-top: 2px solid; display: inline-block; }
	.map-panel { padding: 20px; display: grid; grid-template-columns: 1.2fr 1fr 1.3fr; gap: 0 28px; align-items: start; }
	.map-panel h2 { font-size: 20px; }
	.map-panel h3 { font-size: 11px; font-weight: 600; letter-spacing: .14em; text-transform: uppercase; color: var(--muted-fg); margin: 0; }
	.map-panel > div > h3:not(:first-child) { margin-top: 20px; }
	.map-panel ul { list-style: none; margin: 8px 0 0; padding: 0; }
	.map-panel li { display: flex; align-items: center; gap: 8px; padding: 7px 0; border-bottom: 1px solid var(--border); font-size: 13px; }
	.map-panel li svg { width: 14px; height: 14px; flex: none; }
	.map-panel li .muted { margin-left: auto; font-size: 12px; text-align: right; }
	.muted { color: var(--muted-fg); }
	.note { margin: 12px 0 0; }
	.cause { margin: 10px 0 0; font-size: 13px; }
	.mt2 { margin-top: 2px; }
	.mt8 { margin-top: 8px; }
	.linkish { background: none; border: 0; padding: 0; color: var(--primary); cursor: pointer; text-align: left; }
	@media (max-width: 900px) {
		.map-panel { grid-template-columns: 1fr; gap: 20px; }
		.chips { margin-left: 0; }
	}
</style>
