<script lang="ts">
	import { untrack } from 'svelte';
	import { page } from '$app/state';
	import { goto, replaceState } from '$app/navigation';
	import Icon from '$lib/Icon.svelte';
	import Panel from '$lib/Panel.svelte';
	import Status from '$lib/Status.svelte';
	import HandDrawnNetwork from '$lib/HandDrawnNetwork.svelte';
	import { paths as ICONS } from '$lib/icons';
	import { get, send, SignedOut, type MapBox, type MapFixes, type MapFixReq, type MapFound, type MapLine, type MapView } from '$lib/api';
	import { live } from '$lib/live.svelte';
	import { ago } from '$lib/format';
	import { edgePoint, keptOrder, layout, type Pos } from '$lib/maplayout';
	import { carries, CARRIES_COLOR, CARRIES_WORD, FLOW_ALL_UP_TO, type Carries } from '$lib/flow';

	// The network map, drawn from what the agents see (docs/maps.md): the
	// overview is sites, clusters and remote hosts; a site opens on its
	// hosts (grouped when there are many), a host on what it's linked to.
	// Where you are lives in the address, so a view can be linked to.
	let focus = $state(page.url.searchParams.get('focus') ?? '');
	let tab = $state(page.url.searchParams.get('drawn') ? 'drawn' : 'found');
	let v = $state<MapView | null>(null);
	let error = $state('');
	let sel = $state<string | null>(null);
	let hover = $state<string | null>(null);
	let cw = $state(1000);

	async function load(f: string) {
		try {
			const r = await get<MapView>('/api/v1/map/network' + (f ? '?focus=' + encodeURIComponent(f) : ''));
			if (f === focus) {
				v = r;
				error = '';
			}
		} catch (e) {
			if (e instanceof SignedOut) void live.checkSession();
			else if (String(e).includes(' 404 ')) {
				error = 'That part of the map is gone (a site that split or a host that left). Back to everything.';
				focus = '';
			} else error = String(e);
		}
	}
	$effect(() => {
		const f = focus;
		const p = new URLSearchParams();
		if (f) p.set('focus', f);
		if (tab === 'drawn') p.set('drawn', '1');
		try { replaceState('/map/network' + (p.size ? '?' + p : ''), {}); } catch { /* before the router is up */ }
		if (tab !== 'found') return;
		void load(f);
		const t = setInterval(() => void load(f), 15_000);
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
		return layout(v, keptOrder('cobs.map.network.' + (v.focus ?? ''), v.boxes.map((b) => b.id)), cw - 12);
	});
	const byId = $derived(new Map((v?.boxes ?? []).map((b) => [b.id, b])));

	// Lines: WireGuard always shows; traffic lines are many, so they're faint
	// until you point at or pick one of their ends.
	const lit = $derived(hover ?? sel);
	// WireGuard is green and the Internet blue; traffic takes the colour of
	// what it carries (lib/flow.ts). Dots flow along a line that's up:
	// always for WireGuard and the Internet, for traffic when one of its ends
	// is pointed at.
	const COLOR: Record<string, string> = { wireguard: 'var(--s3)', internet: 'var(--s1)' };
	const tint = (l: MapLine) => (l.up ? (COLOR[l.kind] ?? CARRIES_COLOR[carries(l.ports)]) : 'var(--warning)');
	const flows = (l: MapLine) => l.up && (l.kind === 'traffic' ? touches(l, lit) : (v?.lines.length ?? 0) <= FLOW_ALL_UP_TO || touches(l, lit));
	const LEGEND = $derived.by(() => {
		const seen = new Set((v?.lines ?? []).filter((l) => l.kind === 'traffic').map((l) => carries(l.ports)));
		return (['web', 'mail', 'data', 'messaging', 'other'] as Carries[]).filter((c) => seen.has(c));
	});
	function path(l: MapLine): string {
		const a = lay?.pos[l.a], b = lay?.pos[l.b];
		if (!a || !b) return '';
		const p1 = edgePoint(a, b), p2 = edgePoint(b, a);
		return `M${p1.x} ${p1.y}L${p2.x} ${p2.y}`;
	}
	const touches = (l: MapLine, id: string | null) => !!id && (l.a === id || l.b === id);
	const opacity = (l: MapLine) => {
		if (lit) return touches(l, lit) ? 0.95 : 0.06;
		return l.kind === 'traffic' ? 0.14 : 0.6;
	};
	const width = (l: MapLine) => (l.kind === 'traffic' ? 1.2 : 2) + Math.min(3, Math.log2(l.count ?? 1));
	const lineTitle = (l: MapLine) => {
		const a = byId.get(l.a)?.name ?? l.a, b = byId.get(l.b)?.name ?? l.b;
		const what = l.kind === 'wireguard' ? (l.up ? 'WireGuard' : 'WireGuard, no recent handshake') : l.kind === 'internet' ? 'from the Internet' : 'traffic';
		const n = (l.count ?? 1) > 1 ? ` · ${l.count} links` : '';
		const ports = l.ports?.length ? ` · ${l.ports.map((p) => ':' + p).join(' ')}` : '';
		return `${a} – ${b}: ${what}${n}${ports}`;
	};

	const SEV: Record<string, string> = { good: 'var(--good)', warning: 'var(--warning)', serious: 'var(--serious)', critical: 'var(--critical)' };
	const glyphColor = (st: string) => SEV[st] ?? 'var(--muted-fg)';
	const icon = (st: string) => ICONS[(st in ICONS ? st : 'off') as keyof typeof ICONS];
	const KIND_WORD: Record<string, string> = { site: 'Site', cluster: 'Cluster', host: 'Host', group: 'Group', internet: 'Internet' };
	const kindWord = (k: string) => KIND_WORD[k.replace('elsewhere:', '')] ?? k;
	const rank = (s: string) => ({ good: 0, off: 0, waiting: 1, warning: 2, serious: 3, critical: 4 })[s] ?? 0;

	// What to open when a box is opened.
	function focusOf(b: MapBox): string | null {
		const k = b.kind.replace('elsewhere:', '');
		if (k === 'site' || k === 'group' || k === 'host') return b.id;
		return null;
	}
	function open(b: MapBox) {
		const k = b.kind.replace('elsewhere:', '');
		if (k === 'cluster') return void goto('/clusters');
		const f = focusOf(b);
		if (f) go(f);
	}
	function key(e: KeyboardEvent, b: MapBox) {
		if (e.key === 'Enter') { e.preventDefault(); sel === b.id ? open(b) : (sel = b.id); }
		if (e.key === ' ') { e.preventDefault(); sel = b.id; }
	}

	const selected = $derived(sel ? byId.get(sel) : undefined);
	// Problems first: nothing picked, the panel lists what needs a look.
	const trouble = $derived([...(v?.boxes ?? [])].filter((b) => rank(b.status) >= 2).sort((a, b) => rank(b.status) - rank(a.status) || a.name.localeCompare(b.name)));
	const linksOf = (id: string) => (v?.lines ?? []).filter((l) => touches(l, id));

	// Search: hosts, sites and services anywhere, not only in this view.
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
		if (f.map === 'services') void goto('/map/services?focus=' + encodeURIComponent(f.focus));
		else go(f.focus);
	}
	const drawnToo = $derived(!!live.overview?.features?.topology);

	// Corrections that belong to this map: a site's name, a host hidden.
	let fixes = $state<MapFixes | null>(null);
	let busy = $state(false);
	let fixError = $state('');
	let rename = $state('');
	const canFix = $derived(live.admin && !!fixes?.editable);
	const ours = $derived((fixes?.items ?? []).filter((it) => /^(site|host):/.test(it.undo.id ?? '')));
	async function loadFixes() {
		try { fixes = await get<MapFixes>('/api/v1/map/corrections'); } catch { /* the map still draws */ }
	}
	$effect(() => { if (tab === 'found') void loadFixes(); });
	$effect(() => {
		void sel;
		rename = untrack(() => selected?.name ?? '');
		fixError = '';
	});
	async function fix(r: MapFixReq) {
		busy = true;
		fixError = '';
		try {
			await send('/api/v1/map/corrections', 'POST', r);
			await Promise.all([loadFixes(), load(focus)]);
			return true;
		} catch (e) {
			fixError = e instanceof Error ? e.message : String(e);
			return false;
		} finally {
			busy = false;
		}
	}
</script>

<svelte:head><title>Cairn OBS · Network map</title></svelte:head>

<div class="phead">
	<span class="tile"><Icon name="network" /></span>
	<div>
		<h1>Network map</h1>
		<p>Drawn from what the agents see: sites, the hosts in them and how they're linked. Point at a box to see its links; open it to go in.</p>
	</div>
</div>

{#if drawnToo}
	<div class="tabs" role="tablist">
		<button role="tab" class:on={tab === 'found'} aria-selected={tab === 'found'} onclick={() => (tab = 'found')}>Found</button>
		<button role="tab" class:on={tab === 'drawn'} aria-selected={tab === 'drawn'} onclick={() => (tab = 'drawn')}>Drawn in topology.yaml</button>
	</div>
{/if}

{#if tab === 'drawn'}
	<HandDrawnNetwork />
{:else}
	{#if error}<p class="foot-note" style="color:var(--critical)">{error}</p>{/if}

	{#snippet glyph(st: string, x: number, y: number)}
		<svg {x} {y} width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" style="color:{glyphColor(st)}">{@html icon(st)}</svg>
	{/snippet}
	{#snippet iconLi(st: string)}
		<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" style="color:{glyphColor(st)}">{@html icon(st)}</svg>
	{/snippet}

	<Panel flush>
		<div class="map-bar">
			<nav class="crumbs" aria-label="Where you are">
				{#each v?.crumbs ?? [] as c (c.id)}
					<button class="crumb" onclick={() => go(c.id)}>{c.name}</button><span class="sep">›</span>
				{/each}
				<span class="here">{v?.title ?? '…'}</span>
			</nav>
			<div class="find">
				<input type="search" placeholder="Find a host, site or service" bind:value={typed} aria-label="Find on the maps" />
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
				{:else if v.mapped === 0}
					<div class="none">
						<h2>No map samples yet</h2>
						<p>Each agent sends one a minute through its root helper: the host's interfaces, WireGuard peers and who talks to whom. Agents need <span class="mono">helper_socket</span> set and the helper running (docs/install.md).</p>
					</div>
				{:else}
					<svg viewBox="0 0 {lay.width} {lay.height}" role="img" aria-label="Network map: {v.title}">
						{#each lay.bands as b (b.label + b.y)}
							<rect class="region off" x="16" y={b.y - 10} width={lay.width - 32} height={b.h + 22} rx="14" />
							<text class="region-l" x="32" y={b.y + 8}>{b.label}</text>
						{/each}
						{#each v.lines as l (l.a + l.b + l.kind)}
							{@const moving = flows(l)}
							<path class="lnk" d={path(l)} stroke={tint(l)} stroke-width={width(l)} stroke-dasharray={l.up ? undefined : '6 5'} opacity={moving ? opacity(l) * 0.45 : opacity(l)}><title>{lineTitle(l)}</title></path>
							{#if moving}<path class="flow" d={path(l)} stroke={tint(l)} stroke-width={width(l) + 0.5} opacity={opacity(l)} />{/if}
						{/each}
						{#each v.boxes as b (b.id)}
							{@const p = lay.pos[b.id] as Pos}
							<g class="node {b.kind.replace(':', '-')}" class:sel={sel === b.id} role="button" tabindex="0" aria-label="{kindWord(b.kind)} {b.name}: {b.status}"
								onclick={() => (sel = b.id)} ondblclick={() => open(b)} onkeydown={(e) => key(e, b)}
								onmouseenter={() => (hover = b.id)} onmouseleave={() => (hover = null)} onfocus={() => (hover = b.id)} onblur={() => (hover = null)}>
								<rect class="box" x={p.x - p.w / 2} y={p.y - p.h / 2} width={p.w} height={p.h} rx="10" />
								{#if b.kind !== 'internet'}{@render glyph(b.status, p.x - p.w / 2 + 11, p.y - p.h / 2 + 10)}{/if}
								<text x={p.x - p.w / 2 + (b.kind === 'internet' ? 14 : 32)} y={p.y - p.h / 2 + 22}>{b.name.length > 24 ? b.name.slice(0, 23) + '…' : b.name}</text>
								{#if b.sub}<text class="s" x={p.x - p.w / 2 + 12} y={p.y - p.h / 2 + 41}>{b.sub.length > 32 ? b.sub.slice(0, 31) + '…' : b.sub}</text>{/if}
								{#if b.problems?.length}
									<text class="s bad" x={p.x - p.w / 2 + 12} y={p.y - p.h / 2 + 59}>{b.problems.length} need a look: {b.problems.slice(0, 2).join(', ')}</text>
								{:else if b.ports?.length}
									<text class="m" x={p.x - p.w / 2 + 12} y={p.y + p.h / 2 - 10}>in {b.ports.slice(0, 5).map((x) => ':' + x).join(' ')}</text>
								{/if}
								{#if b.open && !b.kind.startsWith('elsewhere')}<text class="m open" x={p.x + p.w / 2 - 10} y={p.y - p.h / 2 + 22} text-anchor="end">›</text>{/if}
							</g>
						{/each}
					</svg>
					{#if v.cut}<p class="cut">More than fits: some boxes are left out. Open a site or use Find.</p>{/if}
					<div class="map-legend">
						<span><i class="ln" style="border-color:var(--s3)"></i>WireGuard</span>
						<span><i class="ln dash" style="border-color:var(--warning)"></i>no recent handshake</span>
						{#each LEGEND as c (c)}<span><i class="ln" style="border-color:{CARRIES_COLOR[c]}"></i>traffic: {CARRIES_WORD[c]}</span>{/each}
						<span><i class="ln" style="border-color:var(--s1)"></i>from the Internet</span>
						<span>{v.mapped} of {v.hosts} hosts mapped · {ago(v.built)}</span>
					</div>
				{/if}
			</div>

			<aside class="map-panel" aria-live="polite">
				{#if selected}
					{@const b = selected}
					<p class="kind">{kindWord(b.kind)}{b.kind.startsWith('elsewhere') ? ' · outside this view' : ''}</p>
					<h2>{b.name}</h2>
					<div class="mt6"><Status s={b.status} word={b.sub && b.kind === 'host' ? b.sub : undefined} /></div>
					<dl class="kv">
						{#if b.count && b.kind !== 'host'}<dt>{b.kind === 'cluster' ? 'Nodes' : 'Hosts'}</dt><dd>{b.count}</dd>{/if}
						{#if b.via}<dt>Linked via</dt><dd>{b.via}</dd>{/if}
						{#if b.ports?.length}<dt>From the Internet</dt><dd class="mono">{b.ports.map((x) => ':' + x).join(' ')}</dd>{/if}
						{#if b.detail?.addrs}<dt>Addresses</dt><dd class="mono">{(b.detail.addrs as string[]).join(' ') || '—'}</dd>{/if}
						{#if b.detail?.outside_peers}<dt>Other peers</dt><dd>{b.detail.outside_peers} WireGuard peers that aren't our hosts</dd>{/if}
					</dl>
					{#if b.problems?.length}
						<h3>Need a look</h3>
						<ul>{#each b.problems as p (p)}<li>{@render iconLi('warning')}<span>{p}</span></li>{/each}</ul>
					{/if}
					{#if Array.isArray(b.detail?.services) && (b.detail.services as string[]).length}
						<h3>Services here</h3>
						<ul>{#each b.detail.services as string[] as s (s)}<li><a href="/map/services?focus={encodeURIComponent('svc:' + s)}">{s.replace(/^(unit|compose|image|process):/, '')}</a></li>{/each}</ul>
					{/if}
					{#if linksOf(b.id).length}
						<h3>Links</h3>
						<ul>
							{#each linksOf(b.id).slice(0, 30) as l (l.a + l.b + l.kind)}
								{@const other = byId.get(l.a === b.id ? l.b : l.a)}
								<li>{@render iconLi(l.up ? 'good' : 'warning')}<span>{other?.name ?? '?'}</span><span class="muted">{l.kind === 'wireguard' ? 'WireGuard' : l.kind}{l.ports?.length ? ' ' + l.ports.slice(0, 4).map((x) => ':' + x).join(' ') : ''}</span></li>
							{/each}
						</ul>
					{/if}
					{#if canFix && b.kind === 'site'}
						<h3>Correct it</h3>
						<label class="fixrow">Name<span><input bind:value={rename} /><button class="btn sm ghost" disabled={busy || !rename.trim() || rename === b.name} onclick={() => fix({ op: 'name', id: b.id, name: rename })}>Rename</button></span></label>
					{/if}
					{#if fixError}<p class="err">{fixError}</p>{/if}
					<div class="acts">
						{#if canFix && b.kind.replace('elsewhere:', '') === 'host'}<button class="btn sm ghost" disabled={busy} onclick={async () => { if (await fix({ op: 'hide', id: 'host:' + b.name })) sel = null; }}>Hide from the maps</button>{/if}
						{#if b.kind === 'cluster'}<a class="btn sm" href="/clusters">Open clusters <Icon name="arrow" size={14} /></a>
						{:else if focusOf(b) && focusOf(b) !== focus}<button class="btn sm" onclick={() => open(b)}>Open on the map <Icon name="arrow" size={14} /></button>{/if}
						{#if b.kind.replace('elsewhere:', '') === 'host'}<a class="btn sm ghost" href="/hosts/{b.name}">Host page</a>{/if}
					</div>
				{:else if v}
					<h2>{v.title}</h2>
					{#if trouble.length}
						<h3>Need a look</h3>
						<ul>
							{#each trouble.slice(0, 20) as b (b.id)}
								<li>{@render iconLi(b.status)}<button class="linkish" onclick={() => (sel = b.id)}>{b.name}</button><span class="muted">{b.sub ?? ''}</span></li>
							{/each}
						</ul>
					{:else}
						<p class="muted">Everything here is fine.</p>
					{/if}
					{#if ours.length}
						<h3>Corrections ({ours.length})</h3>
						<ul>
							{#each ours as it (it.kind + it.text)}
								<li><span>{it.text}</span>{#if canFix}<button class="btn sm ghost undo" disabled={busy} onclick={() => fix(it.undo)}>Undo</button>{/if}</li>
							{/each}
						</ul>
					{/if}
					<p class="muted">Click a box for its details and links; double-click or press Enter twice to open it.</p>
				{/if}
			</aside>
		</div>
	</Panel>
{/if}

<style>
	.tabs { display: flex; gap: 6px; margin: 0 0 12px; }
	.tabs button { height: 32px; padding: 0 14px; border-radius: 20px; border: 1px solid var(--border); background: var(--card); color: var(--muted-fg); font-size: 13px; cursor: pointer; }
	.tabs button.on { border-color: var(--primary); color: var(--foreground); background: var(--accent); }
	.map-bar { display: flex; align-items: center; gap: 12px; padding: 10px 14px; border-bottom: 1px solid var(--border); flex-wrap: wrap; }
	.crumbs { display: flex; align-items: center; gap: 6px; font-size: 14px; min-width: 0; flex: 1; flex-wrap: wrap; }
	.crumb { background: none; border: 0; padding: 0; color: var(--primary); cursor: pointer; font: inherit; }
	.sep { color: var(--muted-fg); }
	.here { font-weight: 600; }
	.find { position: relative; width: min(320px, 100%); }
	.find input { width: 100%; height: 32px; padding: 0 10px; border-radius: 6px; border: 1px solid var(--input); background: var(--background); color: var(--foreground); font-size: 13px; }
	.found { position: absolute; z-index: 5; left: 0; right: 0; top: 36px; list-style: none; margin: 0; padding: 4px; background: var(--card); border: 1px solid var(--border); border-radius: 8px; box-shadow: 0 8px 24px rgb(0 0 0 / 0.25); max-height: 320px; overflow-y: auto; }
	.found button { display: block; width: 100%; text-align: left; padding: 7px 8px; border: 0; background: none; color: var(--foreground); font-size: 13px; cursor: pointer; border-radius: 4px; }
	.found button:hover { background: var(--accent); }
	.found .k { display: inline-block; min-width: 54px; color: var(--muted-fg); font: 500 11px var(--mono); text-transform: uppercase; }
	.map-wrap { display: grid; grid-template-columns: minmax(0, 1fr) 320px; }
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
	.map-legend i.ln { width: 18px; border-top: 2px solid; display: inline-block; margin: 0; }
	.map-legend i.ln.dash { border-top-style: dashed; }
	.region { fill: color-mix(in oklab, var(--primary) 3%, transparent); stroke: var(--input); }
	.region.off { fill: none; stroke-dasharray: 6 5; }
	.region-l { fill: var(--muted-fg); font: 600 10px var(--mono); letter-spacing: .16em; text-transform: uppercase; }
	.lnk { fill: none; stroke-linecap: round; transition: opacity .15s; }
	.node { cursor: pointer; outline: none; }
	.node .box { fill: var(--card); stroke: var(--input); stroke-width: 1.2; }
	.node.site .box, .node.cluster .box { stroke-width: 1.6; }
	.node.group .box { stroke-dasharray: 5 3; }
	.node.internet .box { fill: color-mix(in oklab, var(--s1) 10%, var(--card)); }
	.node.elsewhere-site .box, .node.elsewhere-host .box { fill: none; stroke-dasharray: 4 4; }
	.node:hover .box, .node:focus-visible .box { stroke: var(--muted-fg); }
	.node.sel .box { stroke: var(--primary); stroke-width: 2; }
	.node text { fill: var(--foreground); font: 600 13px var(--sans); }
	.node text.s { fill: var(--muted-fg); font: 400 11px var(--sans); }
	.node text.s.bad { fill: var(--warning); }
	.node text.m { fill: var(--muted-fg); font: 500 11px var(--mono); }
	.node text.open { font-size: 16px; }
	.map-panel { padding: 20px; max-height: 760px; overflow-y: auto; }
	.map-panel h2 { font-size: 20px; margin: 0; }
	.map-panel .kind { margin: 0 0 4px; font: 600 10px var(--mono); letter-spacing: .14em; text-transform: uppercase; color: var(--muted-fg); }
	.map-panel h3 { font-size: 11px; font-weight: 600; letter-spacing: .14em; text-transform: uppercase; color: var(--muted-fg); margin-top: 20px; }
	.map-panel ul { list-style: none; margin: 8px 0 0; padding: 0; }
	.map-panel li { display: flex; align-items: center; gap: 8px; padding: 7px 0; border-bottom: 1px solid var(--border); font-size: 13px; }
	.map-panel li :global(svg) { width: 14px; height: 14px; flex: none; }
	.map-panel li .muted { margin-left: auto; font-size: 12px; text-align: right; }
	.linkish { background: none; border: 0; padding: 0; color: var(--primary); cursor: pointer; font: inherit; text-align: left; }
	.muted { color: var(--muted-fg); }
	.map-panel p.muted { margin: 12px 0 0; }
	.kv { display: grid; grid-template-columns: 110px 1fr; gap: 10px 14px; margin: 14px 0 0; font-size: 13px; }
	.kv dt { color: var(--muted-fg); }
	.kv dd { margin: 0; word-break: break-word; }
	.mt6 { margin-top: 6px; }
	.fixrow { display: grid; gap: 4px; font-size: 12px; color: var(--muted-fg); margin-top: 8px; }
	.fixrow span { display: flex; gap: 6px; }
	.fixrow input { flex: 1; height: 30px; padding: 0 10px; border-radius: 6px; border: 1px solid var(--input); background: var(--background); color: var(--foreground); font-size: 13px; }
	.undo { margin-left: auto; height: 26px; }
	.err { color: var(--critical); font-size: 13px; }
	.acts { display: flex; gap: 8px; flex-wrap: wrap; margin-top: 16px; }
	.sm { height: 30px; padding: 0 12px; font-size: 13px; }
	@media (max-width: 1100px) {
		.map-wrap { grid-template-columns: 1fr; }
		.map-canvas { border-right: 0; border-bottom: 1px solid var(--border); min-height: 0; }
		.map-panel { max-height: none; }
	}
</style>
