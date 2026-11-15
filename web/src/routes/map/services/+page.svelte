<script lang="ts">
	import { untrack } from 'svelte';
	import { page } from '$app/state';
	import { goto, replaceState } from '$app/navigation';
	import Icon from '$lib/Icon.svelte';
	import Panel from '$lib/Panel.svelte';
	import Status from '$lib/Status.svelte';
	import { paths as ICONS } from '$lib/icons';
	import {
		get, send, SignedOut,
		type MapBox, type MapFixes, type MapFixReq, type MapFound, type MapFoundList, type MapLine, type MapView
	} from '$lib/api';
	import { live } from '$lib/live.svelte';
	import { carries, CARRIES_COLOR, CARRIES_WORD, FLOW_ALL_UP_TO, type Carries } from '$lib/flow';
	import { ago } from '$lib/format';
	import { GAP, edgePoint, keptOrder, layout, untangle, type Pos } from '$lib/maplayout';

	// The service map, drawn from what the agents see (docs/maps.md): every
	// service in tiers, what each needs (learned from connections, or
	// declared), and a focused view of one service: what it needs and what
	// needs it, a few steps each way. Admins correct what was found here.
	let focus = $state(page.url.searchParams.get('focus') ?? '');
	let steps = $state(Number(page.url.searchParams.get('steps')) || 3);
	let v = $state<MapView | null>(null);
	let error = $state('');
	let sel = $state<string | null>(null); // a box id, or "line:<a>|<b>"
	let hover = $state<string | null>(null);
	let cw = $state(1000);

	async function load(f: string, n: number) {
		try {
			const q = new URLSearchParams();
			if (f) q.set('focus', f);
			if (f.startsWith('svc:') && n !== 3) q.set('steps', String(n));
			const r = await get<MapView>('/api/v1/map/services' + (q.size ? '?' + q : ''));
			if (f === focus) {
				v = r;
				error = '';
			}
		} catch (e) {
			if (e instanceof SignedOut) void live.checkSession();
			else if (String(e).includes(' 404 ')) {
				error = 'That service isn’t on the map any more (hidden, merged or gone). Back to every service.';
				focus = '';
			} else error = String(e);
		}
	}
	$effect(() => {
		const f = focus, n = steps;
		const p = new URLSearchParams();
		if (f) p.set('focus', f);
		if (f.startsWith('svc:') && n !== 3) p.set('steps', String(n));
		try { replaceState('/map/services' + (p.size ? '?' + p : ''), {}); } catch { /* before the router is up */ }
		void load(f, n);
		const t = setInterval(() => void load(f, n), 15_000);
		return () => clearInterval(t);
	});
	function go(f: string) {
		if (f === focus) return;
		v = null;
		sel = null;
		focus = f;
	}

	const lay = $derived.by(() => {
		if (!v) return null;
		return layout(v, keptOrder('cobs.map.services.' + (v.focus ?? ''), untangle(v)), cw - 12, true);
	});
	const byId = $derived(new Map((v?.boxes ?? []).map((b) => [b.id, b])));

	// Lines: what's needed is below. A line's colour says what it carries
	// (lib/flow.ts) unless there's trouble at its lower end, which it then
	// takes; dots flow along it toward what's needed. A learned one only
	// seen so far is dashed, a fading one dotted, and neither flows.
	const SEV: Record<string, string> = { good: 'var(--good)', warning: 'var(--warning)', serious: 'var(--serious)', critical: 'var(--critical)' };
	const lineKey = (l: MapLine) => `line:${l.a}|${l.b}`;
	const lit = $derived(hover ?? sel);
	const touches = (l: MapLine, id: string | null) => !!id && (l.a === id || l.b === id || lineKey(l) === id);
	const SKIP = 100; // further than this from a box to the next band's top: a band lies between
	function path(l: MapLine): string {
		const a = lay?.pos[l.a], b = lay?.pos[l.b];
		if (!a || !b) return '';
		// To a box below its band's first row, or past a band in between:
		// over to the gap left of its column, down it and in from the side,
		// so the line never crosses the boxes on the way.
		const sy = a.y + a.h / 2;
		if (b.top !== undefined && sy < b.top && ((b.row ?? 0) > 0 || b.top - sy > SKIP)) {
			const sx = a.x, gx = b.x - (b.cell ?? b.w) / 2 - GAP / 2, yh = Math.min(b.top - 9, sy + 40), my = (sy + yh) / 2, r = 8;
			return `M${sx} ${sy}C${sx} ${my} ${gx} ${my} ${gx} ${yh}L${gx} ${b.y - r}Q${gx} ${b.y} ${gx + r} ${b.y}L${b.x - b.w / 2} ${b.y}`;
		}
		const p1 = edgePoint(a, b), p2 = edgePoint(b, a);
		if (Math.abs(p2.y - p1.y) < 4) return `M${p1.x} ${p1.y}L${p2.x} ${p2.y}`;
		const dy = (p2.y - p1.y) / 2;
		return `M${p1.x} ${p1.y}C${p1.x} ${p1.y + dy} ${p2.x} ${p2.y - dy} ${p2.x} ${p2.y}`;
	}
	const stroke = (l: MapLine) => SEV[byId.get(l.b)?.status === 'good' ? '' : (byId.get(l.b)?.status ?? '')] ?? CARRIES_COLOR[carries(l.ports)];
	// The legend names what this view's lines carry, in a fixed order.
	const LEGEND = $derived.by(() => {
		const seen = new Set((v?.lines ?? []).map((l) => carries(l.ports)));
		return (['web', 'mail', 'data', 'messaging', 'other'] as Carries[]).filter((c) => seen.has(c));
	});
	const flows = (l: MapLine) => l.stage !== 'seen' && l.stage !== 'fading' && ((v?.lines.length ?? 0) <= FLOW_ALL_UP_TO || touches(l, lit));
	const dash = (l: MapLine) => (l.stage === 'seen' ? '6 5' : l.stage === 'fading' ? '2 5' : undefined);
	const opacity = (l: MapLine) => {
		if (lit) return touches(l, lit) ? 0.95 : 0.08;
		return l.stage === 'fading' ? 0.25 : 0.55;
	};
	const STAGE_WORD: Record<string, string> = {
		declared: 'declared', steady: 'learned, steady', seen: 'learned, seen so far', fading: 'learned, not seen for two weeks'
	};
	const ports = (l: MapLine) => (l.ports?.length ? l.ports.map((p) => ':' + p).join(' ') : '');
	const nameOf = (id: string) => byId.get(id)?.name ?? id.replace(/^svc:/, '');
	const lineTitle = (l: MapLine) => `${nameOf(l.a)} needs ${nameOf(l.b)} ${ports(l)} · ${STAGE_WORD[l.stage ?? ''] ?? ''}`;

	const glyphColor = (st: string) => SEV[st] ?? 'var(--muted-fg)';
	const icon = (st: string) => ICONS[(st in ICONS ? st : 'off') as keyof typeof ICONS];
	const rank = (s: string) => ({ good: 0, off: 0, waiting: 1, warning: 2, serious: 3, critical: 4 })[s] ?? 0;

	function open(b: MapBox) {
		if (b.kind === 'group' || b.kind.endsWith('service')) go(b.id);
	}
	function key(e: KeyboardEvent, b: MapBox) {
		if (e.key === 'Enter') { e.preventDefault(); sel === b.id ? open(b) : (sel = b.id); }
		if (e.key === ' ') { e.preventDefault(); sel = b.id; }
	}

	// Focused on a service, the panel shows it until something else is picked.
	const shown = $derived(sel ?? (v?.level === 'service' ? (v.focus ?? null) : null));
	const selected = $derived(shown && !shown.startsWith('line:') ? byId.get(shown) : undefined);
	const selLine = $derived(sel?.startsWith('line:') ? v?.lines.find((l) => lineKey(l) === sel) : undefined);
	const trouble = $derived([...(v?.boxes ?? [])].filter((b) => rank(b.status) >= 2).sort((a, b) => rank(b.status) - rank(a.status) || a.name.localeCompare(b.name)));
	const needs = (id: string) => (v?.lines ?? []).filter((l) => l.a === id);
	const neededBy = (id: string) => (v?.lines ?? []).filter((l) => l.b === id);
	type Detail = { source?: string; kind?: string; parts?: { host: string; owner: string }[]; hosts?: string[]; host_count?: number; ports?: number[]; outside?: string[]; declared?: boolean };
	const detail = (b: MapBox) => (b.detail ?? {}) as Detail;
	const svcId = (boxId: string) => boxId.replace(/^svc:/, '');
	const isSvc = (boxId: string) => boxId.startsWith('svc:');
	const SOURCE_WORD: Record<string, string> = {
		compose: 'a compose project', unit: 'a systemd unit', process: 'a process', image: 'containers from one image',
		declared: 'declared in topology.yaml', outside: 'outside, added by hand', page: 'made on this page',
		kubernetes: 'from a Kubernetes cluster'
	};
	// A cluster's part ("k8s:shop/deployment/web") is on the Workloads page,
	// not a host's.
	const k8sLink = (cluster: string, owner: string) => {
		const [ns, ...rest] = owner.slice(4).split('/');
		const name = rest.at(-1) ?? '';
		return `/workloads?${new URLSearchParams({ source: cluster, ns, q: name })}`;
	};

	// Search: hosts, sites and services anywhere.
	let typed = $state('');
	let found = $state<MapFound[]>([]);
	$effect(() => {
		const q = typed.trim();
		if (!q) { found = []; return; }
		const d = setTimeout(async () => {
			try { found = (await get<{ found: MapFound[] }>('/api/v1/map/find?q=' + encodeURIComponent(q))).found; } catch { found = []; }
		}, 200);
		return () => clearTimeout(d);
	});
	function pickFound(f: MapFound) {
		typed = '';
		found = [];
		if (f.map === 'network') void goto('/map/network?focus=' + encodeURIComponent(f.focus));
		else go(f.focus);
	}

	// Corrections, and what's new since this browser last looked.
	let fixes = $state<MapFixes | null>(null);
	let list = $state<MapFoundList | null>(null);
	let busy = $state(false);
	let fixError = $state('');
	const LOOKED = 'cobs.map.services.looked', DISMISSED = 'cobs.map.services.dismissed';
	const kept = (k: string, d: string) => { try { return localStorage.getItem(k) ?? d; } catch { return d; } };
	const keep = (k: string, val: string) => { try { localStorage.setItem(k, val); } catch { /* nowhere to keep it */ } };
	let looked = $state(Number(kept(LOOKED, '0')));
	let dismissed = $state<string[]>(JSON.parse(kept(DISMISSED, '[]')));
	const canFix = $derived(live.admin && !!fixes?.editable);

	async function loadFixes() {
		try { fixes = await get<MapFixes>('/api/v1/map/corrections'); } catch { /* the map still draws */ }
	}
	async function loadFound() {
		try { list = await get<MapFoundList>('/api/v1/map/found' + (looked ? '?since=' + looked : '')); } catch { /* likewise */ }
	}
	$effect(() => {
		void loadFixes();
		void loadFound();
		const t = setInterval(() => void loadFound(), 60_000);
		return () => clearInterval(t);
	});
	const newServices = $derived((list?.services ?? []).filter((s) => !dismissed.includes('svc:' + s.id)));
	const newEdges = $derived((list?.edges ?? []).filter((e) => !dismissed.includes(`edge:${e.from}|${e.to}|${e.port}`)));
	function markSeen() {
		looked = Date.now();
		keep(LOOKED, String(looked));
		dismissed = [];
		keep(DISMISSED, '[]');
		void loadFound();
	}
	function dismiss(k: string) {
		dismissed = [...dismissed, k].slice(-500);
		keep(DISMISSED, JSON.stringify(dismissed));
	}

	async function fix(...reqs: MapFixReq[]) {
		busy = true;
		fixError = '';
		try {
			for (const r of reqs) await send('/api/v1/map/corrections', 'POST', r);
			await Promise.all([loadFixes(), loadFound(), load(focus, steps)]);
			return true;
		} catch (e) {
			fixError = e instanceof Error ? e.message : String(e);
			return false;
		} finally {
			busy = false;
		}
	}
	// A line folds an edge per port; a verdict covers each.
	function edgeFix(l: MapLine, verdict: '' | 'confirm' | 'reject') {
		const from = svcId(l.a), to = svcId(l.b);
		const ps = l.ports?.length ? l.ports : [0];
		return fix(...ps.map((port) => ({ op: 'edge', from, to, port, verdict }) as MapFixReq));
	}

	// The forms in the side panel, for the service picked.
	let rename = $state('');
	let tier = $state('');
	let mergeQ = $state('');
	let mergeHits = $state<MapFound[]>([]);
	let allParts = $state(false);
	let moving = $state<string | null>(null); // "<host> <owner>"
	let moveTo = $state('');
	let outsideForm = $state<{ name: string; addrs: string; neededBy: string } | null>(null);
	$effect(() => {
		void shown; // only when the pick changes, not on every refresh
		const b = untrack(() => selected);
		rename = b?.name ?? '';
		tier = '';
		mergeQ = '';
		moving = null;
		allParts = false;
		fixError = '';
	});
	$effect(() => {
		const q = mergeQ.trim();
		if (!q) { mergeHits = []; return; }
		const d = setTimeout(async () => {
			try {
				const r = await get<{ found: MapFound[] }>('/api/v1/map/find?q=' + encodeURIComponent(q));
				mergeHits = r.found.filter((f) => f.map === 'services' && f.focus !== selected?.id).slice(0, 8);
			} catch { mergeHits = []; }
		}, 200);
		return () => clearTimeout(d);
	});
	async function addOutside() {
		if (!outsideForm) return;
		const addrs = outsideForm.addrs.split(/[\s,]+/).filter(Boolean);
		const needed = outsideForm.neededBy ? [outsideForm.neededBy] : [];
		if (await fix({ op: 'outside', outside: { name: outsideForm.name, addrs, needed_by: needed } })) outsideForm = null;
	}
	const fmtDay = (s: string) => new Date(s).toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
</script>

<svelte:head><title>Cairn OBS · Service map</title></svelte:head>

<div class="phead">
	<span class="tile"><Icon name="services" /></span>
	<div>
		<h1>Service map</h1>
		<p>What depends on what, found from who talks to whom. Click a service for what it needs and what needs it; open it to see that alone.</p>
	</div>
</div>

{#if error}<p class="foot-note" style="color:var(--critical)">{error}</p>{/if}

{#snippet glyph(st: string, x: number, y: number)}
	<svg {x} {y} width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" style="color:{glyphColor(st)}">{@html icon(st)}</svg>
{/snippet}
{#snippet iconLi(st: string)}
	<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" style="color:{glyphColor(st)}">{@html icon(st)}</svg>
{/snippet}
{#snippet edgeRow(l: MapLine, otherId: string)}
	<li>
		{@render iconLi(byId.get(otherId)?.status ?? 'off')}
		<button class="linkish" onclick={() => (sel = otherId)}>{nameOf(otherId)}</button>
		<span class="muted">{ports(l)} · {l.stage === 'declared' ? 'declared' : l.stage}</span>
		{#if canFix && isSvc(l.a) && isSvc(l.b) && l.stage !== 'declared'}
			<button class="mini" disabled={busy} title="Confirm: it's a real dependency" onclick={() => edgeFix(l, 'confirm')}>✓</button>
			<button class="mini" disabled={busy} title="Not a dependency" onclick={() => edgeFix(l, 'reject')}>✕</button>
		{/if}
	</li>
{/snippet}

<Panel flush>
	<div class="map-bar">
		<nav class="crumbs" aria-label="Where you are">
			{#each v?.crumbs ?? [] as c (c.id)}
				<button class="crumb" onclick={() => go(c.id)}>{c.name}</button><span class="sep">›</span>
			{/each}
			<span class="here">{v?.title ?? '…'}</span>
		</nav>
		{#if focus.startsWith('svc:')}
			<label class="steps">Steps each way
				<select bind:value={steps}>{#each [1, 2, 3, 4, 5] as n (n)}<option value={n}>{n}</option>{/each}</select>
			</label>
		{/if}
		<div class="find">
			<input type="search" placeholder="Find a service, host or site" bind:value={typed} aria-label="Find on the maps" />
			{#if found.length}
				<ul class="found" role="listbox">
					{#each found as f (f.map + f.focus)}
						<li><button role="option" aria-selected="false" onclick={() => pickFound(f)}><span class="k">{f.kind}</span>{f.name}{#if f.where}<span class="muted"> · {f.where}</span>{/if}</button></li>
					{/each}
				</ul>
			{/if}
		</div>
	</div>
	<div class="map-wrap">
		<div class="map-canvas" bind:clientWidth={cw}>
			{#if !v || !lay}
				<p class="empty">Loading…</p>
			{:else if v.boxes.length === 0}
				<div class="none">
					<h2>No services found yet</h2>
					<p>A service is a compose project, a systemd unit that listens on a port, or containers from one image. They show up once the agents send map samples (once a minute, through the root helper), and what each needs follows as they talk to each other.</p>
				</div>
			{:else}
				<svg viewBox="0 0 {lay.width} {lay.height}" role="img" aria-label="Service map: {v.title}">
					<defs>
						<marker id="arr" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse">
							<path d="M0 0L10 5L0 10z" fill="context-stroke" />
						</marker>
					</defs>
					{#each lay.bands as b (b.label + b.y)}
						<rect class="region off" x="16" y={b.y - 10} width={lay.width - 32} height={b.h + 22} rx="14" />
						<text class="region-l" x="32" y={b.y + 8}>{b.label}</text>
					{/each}
					{#each v.lines as l (lineKey(l))}
						{@const moving = flows(l)}
						<path class="lnk" d={path(l)} stroke={stroke(l)} stroke-width={l.stage === 'declared' || l.stage === 'steady' ? 2 : 1.5}
							stroke-dasharray={dash(l)} opacity={moving ? opacity(l) * 0.45 : opacity(l)} marker-end="url(#arr)" />
						{#if moving}<path class="flow" d={path(l)} stroke={stroke(l)} stroke-width="2.5" opacity={opacity(l)} />{/if}
						<path class="hit" d={path(l)} role="button" tabindex="-1" aria-label={lineTitle(l)}
							onclick={() => (sel = lineKey(l))} onkeydown={(e) => e.key === 'Enter' && (sel = lineKey(l))}
							onmouseenter={() => (hover = lineKey(l))} onmouseleave={() => (hover = null)}><title>{lineTitle(l)}</title></path>
					{/each}
					{#each v.boxes as b (b.id)}
						{@const p = lay.pos[b.id] as Pos}
						<g class="node {b.kind.replace(':', '-')}" class:sel={shown === b.id} class:out={detail(b).source === 'outside'} role="button" tabindex="0" aria-label="{b.name}: {b.status}"
							onclick={() => (sel = b.id)} ondblclick={() => open(b)} onkeydown={(e) => key(e, b)}
							onmouseenter={() => (hover = b.id)} onmouseleave={() => (hover = null)} onfocus={() => (hover = b.id)} onblur={() => (hover = null)}>
							<rect class="box" x={p.x - p.w / 2} y={p.y - p.h / 2} width={p.w} height={p.h} rx="10" />
							{@render glyph(b.status, p.x - p.w / 2 + 11, p.y - p.h / 2 + 10)}
							<text x={p.x - p.w / 2 + 32} y={p.y - p.h / 2 + 22}>{b.name.length > 22 ? b.name.slice(0, 21) + '…' : b.name}</text>
							{#if b.kind === 'group'}
								<text class="s" x={p.x - p.w / 2 + 12} y={p.y - p.h / 2 + 41}>{b.count} services</text>
								{#if b.problems?.length}<text class="s bad" x={p.x - p.w / 2 + 12} y={p.y - p.h / 2 + 59}>{b.problems.slice(0, 2).join(', ')}</text>{/if}
							{:else}
								<text class="s" x={p.x - p.w / 2 + 12} y={p.y - p.h / 2 + 41}>{(b.sub ?? '').length > 30 ? b.sub!.slice(0, 29) + '…' : b.sub}</text>
								<text class="m" x={p.x - p.w / 2 + 12} y={p.y + p.h / 2 - 10}>{detail(b).kind ?? ''}{b.count && b.count > 1 ? ` · ${b.count} parts` : ''}{b.ports?.length ? ` · in ${b.ports.slice(0, 3).map((x) => ':' + x).join(' ')}` : ''}</text>
							{/if}
						</g>
					{/each}
				</svg>
				{#if v.cut}<p class="cut">More than fits: some boxes are left out. Open a group or use Find.</p>{/if}
				<div class="map-legend">
					{#each LEGEND as c (c)}<span><i class="ln" style="border-color:{CARRIES_COLOR[c]}"></i>{CARRIES_WORD[c]}</span>{/each}
					<span><i class="ln" style="border-color:var(--warning)"></i>needs something in trouble</span>
					<span><i class="ln"></i>declared or steady, flowing</span>
					<span><i class="ln dash"></i>seen so far</span>
					<span><i class="ln dot"></i>fading</span>
					<span>an arrow points at what's needed</span>
					<span>{v.mapped} of {v.hosts} hosts mapped · {ago(v.built)}</span>
				</div>
			{/if}
		</div>

		<aside class="map-panel" aria-live="polite">
			{#if fixError}<p class="err">{fixError}</p>{/if}
			{#if sel}<button class="back linkish" onclick={() => (sel = null)}>← {v?.level === 'service' ? v.title : 'Back to the list'}</button>{/if}
			{#if selLine}
				{@const l = selLine}
				<p class="kind">Dependency</p>
				<h2>{nameOf(l.a)} needs {nameOf(l.b)}</h2>
				<dl class="kv">
					{#if ports(l)}<dt>Port</dt><dd class="mono">{ports(l)}</dd>{/if}
					<dt>How we know</dt><dd>{STAGE_WORD[l.stage ?? ''] ?? '—'}{l.count && l.count > 1 ? ` · ${l.count} edges` : ''}</dd>
				</dl>
				<p class="muted">{l.stage === 'seen'
					? 'Seen in its connections, but not yet in three separate hours in a week. Until then it doesn’t group alerts.'
					: l.stage === 'steady' ? 'Seen in three separate hours within a week. Steady edges will group alerts under one incident (maps step 6).'
					: l.stage === 'fading' ? 'Not seen for two weeks. It goes at 30 days unless it’s confirmed.'
					: 'Declared: in topology.yaml, or confirmed here.'}</p>
				{#if canFix && isSvc(l.a) && isSvc(l.b)}
					<div class="acts">
						{#if l.stage !== 'declared'}<button class="btn sm" disabled={busy} onclick={() => edgeFix(l, 'confirm')}>Confirm</button>{/if}
						<button class="btn sm ghost" disabled={busy} onclick={() => edgeFix(l, 'reject')}>Not a dependency</button>
					</div>
				{/if}
			{:else if selected}
				{@const b = selected}
				{@const d = detail(b)}
				<p class="kind">{b.kind === 'group' ? 'Group' : (SOURCE_WORD[d.source ?? ''] ?? 'Service')}{b.kind.startsWith('elsewhere') ? ' · outside this view' : ''}</p>
				<h2>{b.name}</h2>
				<div class="mt6"><Status s={b.status} word={b.sub} /></div>
				{#if b.kind === 'group'}
					<p class="muted">{b.count} services. Open it to see them.</p>
					<div class="acts"><button class="btn sm" onclick={() => open(b)}>Open <Icon name="arrow" size={14} /></button></div>
				{:else}
					<dl class="kv">
						{#if d.kind}<dt>Looks like</dt><dd>{d.kind}</dd>{/if}
						<dt>Found as</dt><dd class="mono">{svcId(b.id)}</dd>
						{#if b.tier}<dt>Tier</dt><dd>{b.tier}</dd>{/if}
						{#if d.ports?.length}<dt>Listens on</dt><dd class="mono">{d.ports.map((x) => ':' + x).join(' ')}</dd>{/if}
						{#if b.ports?.length}<dt>From the Internet</dt><dd class="mono">{b.ports.map((x) => ':' + x).join(' ')}</dd>{/if}
					</dl>
					{#if needs(b.id).length}
						<h3>Needs</h3>
						<ul>{#each needs(b.id) as l (lineKey(l))}{@render edgeRow(l, l.b)}{/each}</ul>
					{/if}
					{#if neededBy(b.id).length}
						<h3>Needed by</h3>
						<ul>{#each neededBy(b.id) as l (lineKey(l))}{@render edgeRow(l, l.a)}{/each}</ul>
					{/if}
					{#if d.parts?.length}
						<h3>Parts{d.parts.length < (b.count ?? 0) ? ` (first ${d.parts.length} of ${b.count})` : ''}</h3>
						<ul>
							{#each allParts ? d.parts : d.parts.slice(0, 8) as pt (pt.host + pt.owner)}
								{@const k = pt.host + ' ' + pt.owner}
								<li>
									{#if pt.owner.startsWith('k8s:')}
										<a href={k8sLink(pt.host, pt.owner)}>{pt.host}</a><span class="muted mono">{pt.owner.slice(4)}</span>
									{:else}
										<a href="/hosts/{pt.host}">{pt.host}</a><span class="muted mono">{pt.owner.replace(/^(container|unit|process):/, '')}</span>
									{/if}
									{#if canFix}<button class="mini" title="Move it to another service" onclick={() => { moving = moving === k ? null : k; moveTo = ''; }}>⇄</button>{/if}
								</li>
								{#if moving === k}
									<li class="form">
										<input placeholder="Service it belongs to (a new name makes one)" bind:value={moveTo} />
										<button class="btn sm" disabled={busy || !moveTo.trim()} onclick={async () => {
											const hit = v?.boxes.find((x) => x.name === moveTo.trim() && isSvc(x.id));
											if (await fix(hit ? { op: 'move', host: pt.host, owner: pt.owner, into: svcId(hit.id) } : { op: 'move', host: pt.host, owner: pt.owner, name: moveTo.trim() })) moving = null;
										}}>Move</button>
									</li>
								{/if}
							{/each}
						</ul>
						{#if d.parts.length > 8 && !allParts}<button class="linkish more" onclick={() => (allParts = true)}>{d.parts.length < (b.count ?? 0) ? `Show the first ${d.parts.length}` : `Show all ${d.parts.length}`}</button>{/if}
					{/if}
					{#if d.outside?.length}
						<h3>Reaches outside</h3>
						<ul>
							{#each d.outside as o (o)}
								<li><span class="mono">{o}</span>
									{#if canFix}<button class="mini wide" onclick={() => (outsideForm = { name: '', addrs: o, neededBy: svcId(b.id) })}>Name it</button>{/if}
								</li>
							{/each}
						</ul>
					{/if}
					<div class="acts">
						{#if b.id !== focus}<button class="btn sm" onclick={() => open(b)}>Focus on it <Icon name="arrow" size={14} /></button>{/if}
					</div>
					{#if canFix}
						<h3>Correct it</h3>
						<div class="fixes">
							<label>Name<span class="row"><input bind:value={rename} /><button class="btn sm ghost" disabled={busy || !rename.trim() || rename === b.name} onclick={() => fix({ op: 'name', id: svcId(b.id), name: rename })}>Rename</button></span></label>
							<label>Tier<span class="row">
								<select bind:value={tier}><option value="">Worked out ({b.tier})</option><option value="edge">Edge</option><option value="apps">Apps</option><option value="data">Data</option></select>
								<button class="btn sm ghost" disabled={busy} onclick={() => fix({ op: 'tier', id: svcId(b.id), tier })}>Set</button></span></label>
							<label>Merge into another service<input placeholder="Find a service" bind:value={mergeQ} /></label>
							{#if mergeHits.length}
								<ul class="hits">{#each mergeHits as h (h.focus)}<li><button class="linkish" disabled={busy} onclick={async () => { if (await fix({ op: 'merge', id: svcId(b.id), into: svcId(h.focus) })) { sel = null; mergeQ = ''; } }}>{h.name}</button><span class="muted">{h.where ?? ''}</span></li>{/each}</ul>
							{/if}
							<button class="btn sm ghost" disabled={busy} onclick={async () => { if (await fix({ op: 'hide', id: svcId(b.id) })) sel = null; }}>Hide from the maps</button>
						</div>
					{/if}
				{/if}
			{:else if v}
				<h2>{v.title}</h2>
				{#if newServices.length || newEdges.length}
					<h3>Found since you last looked</h3>
					<ul>
						{#each newServices as s (s.id)}
							<li class="new">
								<span class="tag">new</span>
								<span class="what"><button class="linkish" onclick={() => (byId.has('svc:' + s.id) ? (sel = 'svc:' + s.id) : go('svc:' + s.id))}>{s.name}</button>
									<small>{s.hosts.slice(0, 2).join(', ')}{s.hosts.length > 2 ? ' …' : ''} · since {fmtDay(s.first_seen)}</small></span>
								<button class="mini" title="Looks right" onclick={() => dismiss('svc:' + s.id)}>✓</button>
								{#if canFix}<button class="mini" title="Hide from the maps" disabled={busy} onclick={() => fix({ op: 'hide', id: s.id })}>✕</button>{/if}
							</li>
						{/each}
						{#each newEdges as e (e.from + e.to + e.port)}
							<li class="new">
								<span class="tag">needs</span>
								<span class="what">{e.from_name} → {e.to_name}{e.port ? ' :' + e.port : ''}
									<small>{e.stage} · since {fmtDay(e.first_seen)}</small></span>
								{#if canFix}
									<button class="mini" title="Confirm" disabled={busy} onclick={() => fix({ op: 'edge', from: e.from, to: e.to, port: e.port, verdict: 'confirm' })}>✓</button>
									<button class="mini" title="Not a dependency" disabled={busy} onclick={() => fix({ op: 'edge', from: e.from, to: e.to, port: e.port, verdict: 'reject' })}>✕</button>
								{:else}
									<button class="mini" title="Seen it" onclick={() => dismiss(`edge:${e.from}|${e.to}|${e.port}`)}>✓</button>
								{/if}
							</li>
						{/each}
					</ul>
					{#if (list?.service_count ?? 0) + (list?.edge_count ?? 0) > newServices.length + newEdges.length}
						<p class="muted">{(list?.service_count ?? 0) + (list?.edge_count ?? 0)} new in all; the newest are listed.</p>
					{/if}
					<div class="acts"><button class="btn sm ghost" onclick={markSeen}>Mark all as seen</button></div>
				{:else if list}
					<p class="muted">Nothing new since {looked ? 'you last looked' : 'last week'}.</p>
				{/if}
				{#if trouble.length}
					<h3>Need a look</h3>
					<ul>
						{#each trouble.slice(0, 20) as b (b.id)}
							<li>{@render iconLi(b.status)}<button class="linkish" onclick={() => (sel = b.id)}>{b.name}</button><span class="muted">{b.sub ?? ''}</span></li>
						{/each}
					</ul>
				{/if}
				{#if fixes?.items?.length}
					<h3>Corrections ({fixes.items.length})</h3>
					<ul>
						{#each fixes.items as it (JSON.stringify(it.undo))}
							<li><span>{it.text}</span>{#if canFix}<button class="mini wide" disabled={busy} onclick={() => fix(it.undo)}>Undo</button>{/if}</li>
						{/each}
					</ul>
					{#if fixes.updated_by}<p class="muted">Last by {fixes.updated_by}, {ago(fixes.updated ?? '')}.</p>{/if}
				{/if}
				{#if canFix}
					<div class="acts"><button class="btn sm ghost" onclick={() => (outsideForm = { name: '', addrs: '', neededBy: '' })}>Add an outside service</button></div>
				{/if}
				<p class="muted">Click a service or a line for its details; double-click or press Enter twice to open it.</p>
			{/if}

			{#if outsideForm}
				<div class="outside">
					<h3>An outside service</h3>
					<p class="muted">Something not ours that services depend on: a payment provider, a relay at another company. Connections to its addresses become dependencies on it.</p>
					<label>Name<input bind:value={outsideForm.name} placeholder="Payments provider" /></label>
					<label>Addresses<input bind:value={outsideForm.addrs} placeholder="203.0.113.10:443 198.51.100.0/24" class="mono" /></label>
					{#if outsideForm.neededBy}<p class="muted">Needed by {nameOf('svc:' + outsideForm.neededBy)}.</p>{/if}
					<div class="acts">
						<button class="btn sm" disabled={busy || !outsideForm.name.trim()} onclick={addOutside}>Add it</button>
						<button class="btn sm ghost" onclick={() => (outsideForm = null)}>Cancel</button>
					</div>
				</div>
			{/if}
		</aside>
	</div>
</Panel>

<style>
	.map-bar { display: flex; align-items: center; gap: 12px; padding: 10px 14px; border-bottom: 1px solid var(--border); flex-wrap: wrap; }
	.crumbs { display: flex; align-items: center; gap: 6px; font-size: 14px; min-width: 0; flex: 1; flex-wrap: wrap; }
	.crumb { background: none; border: 0; padding: 0; color: var(--primary); cursor: pointer; font: inherit; }
	.sep { color: var(--muted-fg); }
	.here { font-weight: 600; }
	.steps { display: flex; align-items: center; gap: 6px; font-size: 12px; color: var(--muted-fg); }
	.steps select, .fixes select { height: 30px; border-radius: 6px; border: 1px solid var(--input); background: var(--background); color: var(--foreground); font-size: 13px; padding: 0 6px; }
	.find { position: relative; width: min(320px, 100%); }
	.find input, .map-panel input { width: 100%; height: 32px; padding: 0 10px; border-radius: 6px; border: 1px solid var(--input); background: var(--background); color: var(--foreground); font-size: 13px; }
	.found { position: absolute; z-index: 5; left: 0; right: 0; top: 36px; list-style: none; margin: 0; padding: 4px; background: var(--card); border: 1px solid var(--border); border-radius: 8px; box-shadow: 0 8px 24px rgb(0 0 0 / 0.25); max-height: 320px; overflow-y: auto; }
	.found button { display: block; width: 100%; text-align: left; padding: 7px 8px; border: 0; background: none; color: var(--foreground); font-size: 13px; cursor: pointer; border-radius: 4px; }
	.found button:hover { background: var(--accent); }
	.found .k { display: inline-block; min-width: 54px; color: var(--muted-fg); font: 500 11px var(--mono); text-transform: uppercase; }
	.map-wrap { display: grid; grid-template-columns: minmax(0, 1fr) 340px; }
	.map-canvas {
		position: relative; min-width: 0; border-right: 1px solid var(--border); min-height: 520px;
		background-image: radial-gradient(color-mix(in oklab, var(--muted-fg) 22%, transparent) 1px, transparent 1px); background-size: 22px 22px;
		padding: 10px 6px 56px;
	}
	.map-canvas svg { display: block; width: 100%; height: auto; }
	.none { padding: 60px 40px; max-width: 560px; }
	.none h2 { font-size: 18px; }
	.none p { color: var(--muted-fg); }
	.cut { margin: 6px 14px; font-size: 12px; color: var(--warning); }
	.map-legend {
		position: absolute; left: 14px; bottom: 12px; right: 14px; width: fit-content; display: flex; gap: 6px 14px; flex-wrap: wrap; font-size: 12px; color: var(--muted-fg);
		background: color-mix(in oklab, var(--card) 88%, transparent); border: 1px solid var(--border); padding: 6px 10px; border-radius: 6px;
	}
	.map-legend span { display: inline-flex; align-items: center; gap: 6px; }
	.map-legend i.ln { width: 18px; border-top: 2px solid var(--muted-fg); display: inline-block; }
	.map-legend i.ln.dash { border-top-style: dashed; }
	.map-legend i.ln.dot { border-top-style: dotted; opacity: .6; }
	.region { fill: none; stroke: var(--input); stroke-dasharray: 6 5; }
	.region-l { fill: var(--muted-fg); font: 600 10px var(--mono); letter-spacing: .16em; text-transform: uppercase; }
	.lnk { fill: none; stroke-linecap: round; transition: opacity .15s; pointer-events: none; }
	.hit { fill: none; stroke: transparent; stroke-width: 12; cursor: pointer; outline: none; }
	.node { cursor: pointer; outline: none; }
	.node .box { fill: var(--card); stroke: var(--input); stroke-width: 1.2; }
	.node.group .box { stroke-dasharray: 5 3; stroke-width: 1.6; }
	.node.out .box { fill: none; stroke-dasharray: 3 3; }
	.node.elsewhere-service .box { fill: none; stroke-dasharray: 4 4; }
	.node:hover .box, .node:focus-visible .box { stroke: var(--muted-fg); }
	.node.sel .box { stroke: var(--primary); stroke-width: 2; }
	.node text { fill: var(--foreground); font: 600 13px var(--sans); }
	.node text.s { fill: var(--muted-fg); font: 400 11px var(--sans); }
	.node text.s.bad { fill: var(--warning); }
	.node text.m { fill: var(--muted-fg); font: 500 11px var(--mono); }
	.map-panel { padding: 20px; max-height: 820px; overflow-y: auto; }
	.map-panel h2 { font-size: 20px; margin: 0; }
	.map-panel .kind { margin: 0 0 4px; font: 600 10px var(--mono); letter-spacing: .14em; text-transform: uppercase; color: var(--muted-fg); }
	.map-panel h3 { font-size: 11px; font-weight: 600; letter-spacing: .14em; text-transform: uppercase; color: var(--muted-fg); margin-top: 20px; }
	.map-panel ul { list-style: none; margin: 8px 0 0; padding: 0; }
	.map-panel li { display: flex; align-items: center; gap: 8px; padding: 7px 0; border-bottom: 1px solid var(--border); font-size: 13px; }
	.map-panel li :global(svg) { width: 14px; height: 14px; flex: none; }
	.map-panel li .muted { margin-left: auto; font-size: 12px; text-align: right; }
	.map-panel li.form { gap: 6px; }
	.map-panel li.new .what { flex: 1; min-width: 0; display: grid; gap: 2px; }
	.map-panel li.new .what small { color: var(--muted-fg); font-size: 11px; }
	.map-panel li.new .tag { font: 600 9px var(--mono); letter-spacing: .1em; text-transform: uppercase; color: var(--primary); border: 1px solid var(--primary); border-radius: 4px; padding: 1px 4px; }
	.mini { flex: none; height: 24px; min-width: 24px; padding: 0 6px; border-radius: 5px; border: 1px solid var(--border); background: var(--card); color: var(--muted-fg); font-size: 12px; cursor: pointer; }
	.mini:hover:not(:disabled) { color: var(--foreground); border-color: var(--muted-fg); }
	.mini.wide { margin-left: auto; }
	.linkish { background: none; border: 0; padding: 0; color: var(--primary); cursor: pointer; font: inherit; text-align: left; }
	.muted { color: var(--muted-fg); }
	.map-panel p.muted { margin: 12px 0 0; font-size: 13px; }
	.more { margin-top: 8px; font-size: 13px; }
	.back { display: block; margin: 0 0 14px; font-size: 13px; }
	.err { color: var(--critical); font-size: 13px; margin: 0 0 12px; }
	.kv { display: grid; grid-template-columns: 110px 1fr; gap: 10px 14px; margin: 14px 0 0; font-size: 13px; }
	.kv dt { color: var(--muted-fg); }
	.kv dd { margin: 0; word-break: break-word; }
	.mt6 { margin-top: 6px; }
	.acts { display: flex; gap: 8px; flex-wrap: wrap; margin-top: 16px; }
	.sm { height: 30px; padding: 0 12px; font-size: 13px; }
	.fixes, .outside { display: grid; gap: 10px; margin-top: 8px; }
	.fixes label, .outside label { display: grid; gap: 4px; font-size: 12px; color: var(--muted-fg); }
	.fixes .row { display: flex; gap: 6px; }
	.fixes .row select { flex: 1; }
	.fixes .hits { margin: 0; }
	.outside { margin-top: 20px; padding-top: 4px; border-top: 1px solid var(--border); }
	@media (max-width: 1100px) {
		.map-wrap { grid-template-columns: 1fr; }
		.map-canvas { border-right: 0; border-bottom: 1px solid var(--border); min-height: 0; }
		.map-panel { max-height: none; }
	}
</style>
