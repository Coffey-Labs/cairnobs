<script lang="ts">
	// The network map drawn by hand in topology.yaml: Cloudflare and the mail
	// names, the mesh's round trips from checks, the alert channels. The
	// discovered map (the page) is the default; this stays beside it while
	// topology.yaml is set.
	import Icon from '$lib/Icon.svelte';
	import Panel from '$lib/Panel.svelte';
	import Status from '$lib/Status.svelte';
	import { paths as ICONS } from '$lib/icons';
	import { topology } from '$lib/topology.svelte';
	import { ago, duration, rate } from '$lib/format';
	import type { NetHost, NetRemote, Severity } from '$lib/api';

	const t = topology();
	const net = $derived(t.data?.network);
	// Hosts outside the site, each on a link of its own to one site host.
	const remotes = $derived<NetRemote[]>(net?.remote ?? []);

	// The demo's drawing: the internet on top, Cloudflare, direct names and
	// the alert channels below it, the hosts in their site box underneath.
	type Box = { x: number; y: number; w: number; h: number };
	const FIXED: Record<string, Box> = {
		internet: { x: 500, y: 58, w: 190, h: 50 },
		cf: { x: 365, y: 172, w: 226, h: 58 },
		direct: { x: 650, y: 172, w: 226, h: 58 },
		alerts: { x: 896, y: 172, w: 172, h: 58 }
	};
	const SLOTS = [{ x: 290, y: 372 }, { x: 710, y: 372 }, { x: 500, y: 548 }];
	const HOST_W = 216, HOST_H = 80;
	// Remote hosts sit in a band of their own under the site box.
	const REMOTE_Y = 724;
	const boxes = $derived.by(() => {
		const b: Record<string, Box> = { ...FIXED };
		(net?.hosts ?? []).forEach((h, i) => (b[h.name] = { ...SLOTS[i % SLOTS.length], w: HOST_W, h: HOST_H }));
		remotes.forEach((r, i) => (b[r.name] = { x: 500 + (i - (remotes.length - 1) / 2) * 260, y: REMOTE_Y, w: HOST_W, h: HOST_H }));
		return b;
	});

	// Healthy links take their layer's colour; anything else its state's.
	const LAYERS = [
		['public', 'Web (via Cloudflare)', 'var(--s1)'],
		['mail', 'Direct (mail)', 'var(--s2)'],
		['mesh', 'WireGuard', 'var(--s3)'],
		['backup', 'Backup flow', 'var(--muted-fg)'],
		['probe', 'Probes & alerts', 'var(--s4)']
	] as const;
	type Layer = (typeof LAYERS)[number][0];
	const LAYER_COLOR = Object.fromEntries(LAYERS.map(([k, , c]) => [k, c])) as Record<Layer, string>;
	let layers = $state<Record<Layer, boolean>>({ public: true, mail: true, mesh: true, backup: false, probe: true });
	const SEV: Record<string, string> = { warning: 'var(--warning)', serious: 'var(--serious)', critical: 'var(--critical)' };
	const lineColor = (st: string, layer: Layer) => (st === 'good' || st === 'waiting' ? LAYER_COLOR[layer] : (SEV[st] ?? 'var(--muted-fg)'));
	const glyphColor = (st: string) => (st === 'good' ? 'var(--good)' : (SEV[st] ?? 'var(--muted-fg)'));
	const rank = (s: string) => ({ good: 0, waiting: 1, warning: 2, serious: 3, critical: 4 })[s] ?? 0;

	// Web lines grow with the host's traffic: 2 px idle, 6 px at 50 Mb/s and up.
	const traffic = (h?: NetHost) => (h ? ((h.rx_bps + h.tx_bps) * 8) / 1e6 : 0);
	const thick = (h?: NetHost) => 2 + Math.min(4, Math.sqrt(traffic(h) / 50) * 4);

	// Where a line leaves a box, heading for another.
	function edge(a: Box, b: Box) {
		const dx = b.x - a.x, dy = b.y - a.y;
		const s = Math.min(a.w / 2 / Math.abs(dx || 1e-6), a.h / 2 / Math.abs(dy || 1e-6));
		return { x: a.x + dx * s, y: a.y + dy * s };
	}
	type Link = { id: string; layer: Layer; d: string; st: string; w: number; label: string; lx: number; ly: number; title: string; dash?: boolean };
	function link(id: string, from: string, to: string, layer: Layer, st: string, w: number, label = '', bend = 0, at = 0.5, title = ''): Link | null {
		const A = boxes[from], B = boxes[to];
		if (!A || !B) return null;
		const p1 = edge(A, B), p2 = edge(B, A);
		let d: string, lx: number, ly: number;
		if (bend) {
			const mx = (p1.x + p2.x) / 2, my = (p1.y + p2.y) / 2, dx = p2.x - p1.x, dy = p2.y - p1.y, len = Math.hypot(dx, dy);
			const cx = mx - (dy / len) * bend, cy = my + (dx / len) * bend;
			d = `M${p1.x} ${p1.y}Q${cx} ${cy} ${p2.x} ${p2.y}`;
			lx = (1 - at) ** 2 * p1.x + 2 * (1 - at) * at * cx + at * at * p2.x;
			ly = (1 - at) ** 2 * p1.y + 2 * (1 - at) * at * cy + at * at * p2.y;
		} else {
			d = `M${p1.x} ${p1.y}L${p2.x} ${p2.y}`;
			lx = p1.x + (p2.x - p1.x) * at;
			ly = p1.y + (p2.y - p1.y) * at;
		}
		return { id, layer, d, st, w, label, lx, ly, title: title || label };
	}
	const ms = (v: number | null) => (v == null ? '—' : v < 10 ? v.toFixed(1) + ' ms' : Math.round(v) + ' ms');
	const beaconHost = $derived(t.data?.services?.find((s) => s.id === 'beacon')?.signals.find((s) => s.host)?.host ?? net?.hosts.at(-1)?.name ?? '');
	const mailWorst = $derived((net?.paths ?? []).filter((p) => p.kind === 'mail').reduce((a, p) => (rank(p.status) > rank(a) ? p.status : a), 'good' as string));

	const links = $derived.by(() => {
		if (!net) return [];
		const out: (Link | null)[] = [];
		const cfSt = net.cloudflare?.status ?? 'good';
		out.push(link('i-cf', 'internet', 'cf', 'public', cfSt === 'waiting' ? 'good' : cfSt, 4, 'HTTPS'));
		out.push(link('i-direct', 'internet', 'direct', 'mail', mailWorst, 3, ':25 :465 :587 :993'));
		for (const p of net.paths) {
			const h = net.hosts.find((x) => x.name === p.host);
			if (p.kind === 'public') out.push(link('cf-' + p.host, 'cf', p.host, 'public', p.status, thick(h), p.status === 'good' ? '' : p.word, 0, 0.35, `Web to ${p.host}: ${p.word}`));
			else out.push(link('mx-' + p.name, 'direct', p.host, 'mail', p.status, 2.5, p.name, 0, 0.72, `${p.name}: ${p.word}`));
		}
		for (const l of net.links) out.push(link('mesh-' + l.hosts.join('-'), l.hosts[0], l.hosts[1], 'mesh', l.status, 3, ms(l.ms), 0, 0.5, `${l.hosts.join(' – ')}: ${l.word}`));
		for (const r of remotes) {
			const label = r.link.ms == null ? r.link.name : `${r.link.name} · ${ms(r.link.ms)}`;
			const l = link('remote-' + r.name, r.link.via, r.name, 'mesh', r.link.status, 3, label, 0, 0.5, `${r.link.via} – ${r.name} over ${r.link.name || 'its own link'}: ${r.link.word}`);
			if (l) out.push({ ...l, dash: true });
		}
		// Every host backs up to both others each night (hosts-baseline backup-mesh.sh).
		const hs = net.hosts.map((h) => h.name);
		hs.forEach((a, i) => {
			const b = hs[(i + 1) % hs.length];
			if (a !== b) out.push(link(`bk-${a}-${b}`, a, b, 'backup', 'good', 2, 'nightly', 70, 0.5, 'Backups, both ways each night: not watched by Cairn OBS yet'));
		});
		out.push(link('alerts', beaconHost, 'alerts', 'probe', 'good', 2, 'alerts out', 220, 0.5, 'obs-beacond sends alerts'));
		return out.filter((x): x is Link => x !== null);
	});

	// The detail panel: click anything.
	let sel = $state<string | null>(null);
	const pick = (id: string) => (sel = id);
	const selected = $derived.by(() => {
		if (sel && boxes[sel]) return sel;
		const worstHost = [...(net?.hosts ?? [])].sort((a, b) => rank(b.status) - rank(a.status))[0];
		return worstHost?.name ?? 'cf';
	});
	const selRemote = $derived(remotes.find((r) => r.name === selected));
	const selHost = $derived<NetHost | undefined>(net?.hosts.find((h) => h.name === selected) ?? selRemote);
	const peers = (name: string) => (net?.links ?? []).filter((l) => l.hosts.includes(name)).map((l) => ({ host: l.hosts.find((h) => h !== name)!, ms: l.ms }));
	const ownLinks = (name: string) => remotes.filter((r) => r.link.via === name);
	const chanWord = (c: { type: string }) => (c.type === 'ntfy' ? 'ntfy' : c.type === 'discord' ? 'Discord' : c.type);
	function key(e: KeyboardEvent, id: string) {
		if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); pick(id); }
	}
	const sev = (s: string) => s as Severity;
	const icon = (st: string) => ICONS[(st in ICONS ? st : 'off') as keyof typeof ICONS];
</script>

{#if t.error}<p class="foot-note" style="color:var(--critical)">{t.error}</p>{/if}

{#snippet glyph(st: string, x: number, y: number)}
	<svg {x} {y} width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" style="color:{glyphColor(st)}">{@html icon(st)}</svg>
{/snippet}
{#snippet iconLi(st: string)}
	<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" style="color:{glyphColor(st)}">{@html icon(st)}</svg>
{/snippet}
{#snippet node(id: string, title: string, sub: string, st: string)}
	{@const b = boxes[id]}
	<g class="node" class:sel={selected === id} role="button" tabindex="0" aria-label="{title}: {st}" onclick={() => pick(id)} onkeydown={(e) => key(e, id)}>
		<rect class="box" x={b.x - b.w / 2} y={b.y - b.h / 2} width={b.w} height={b.h} rx="10" />
		{@render glyph(st, b.x - b.w / 2 + 12, b.y - b.h / 2 + 11)}
		<text x={b.x - b.w / 2 + 34} y={b.y - b.h / 2 + 23}>{title}</text>
		<text class="s" x={b.x - b.w / 2 + 12} y={b.y - b.h / 2 + 42}>{sub}</text>
	</g>
{/snippet}

{#if t.data && !t.data.configured}
	<Panel title="Not set up"><p class="sub">Point <span class="mono">topology</span> in obs-beacond.yaml at a topology file. See docs/formats.md.</p></Panel>
{:else if net}
	<Panel flush>
		<div class="map-wrap">
			<div class="map-canvas">
				<div class="map-tools" role="group" aria-label="Layers">
					{#each LAYERS as [k, label, c] (k)}
						<button class="chip" class:on={layers[k]} aria-pressed={layers[k]} onclick={() => (layers[k] = !layers[k])}><i class="dot" style="background:{c}"></i>{label}</button>
					{/each}
				</div>
				<svg viewBox="0 -40 1000 {remotes.length ? 830 : 680}" preserveAspectRatio="xMidYMid meet" role="img" aria-label="Network map">
					<rect class="region" x="40" y="268" width="920" height="350" rx="16" />
					<text class="region-l" x="58" y="292">{net.site} · private network</text>
					{#if remotes.length}
						<rect class="region off" x="40" y="650" width="920" height="136" rx="16" />
						<text class="region-l" x="58" y="674">Off site · own WireGuard links</text>
					{/if}

					{#each links.filter((l) => layers[l.layer]) as l (l.id)}
						{@const c = lineColor(l.st, l.layer)}
						<path class="lnk" d={l.d} stroke={c} stroke-width={l.w} stroke-dasharray={l.dash ? '7 6' : undefined} opacity={l.st === 'good' || l.st === 'waiting' ? 0.5 : 0.95}><title>{l.title}</title></path>
						<path class="lnk flow" d={l.d} stroke={c} stroke-width={l.w} style="animation-duration:{l.layer === 'backup' ? 3 : l.layer === 'mail' ? 1.8 : 1.4}s" />
					{/each}
					{#each links.filter((l) => layers[l.layer] && l.label) as l (l.id)}
						<text class="lnk-l" x={l.lx} y={l.ly + 4} text-anchor="middle" style={l.st !== 'good' && l.st !== 'waiting' ? `fill:${lineColor(l.st, l.layer)}` : ''}>{l.label}</text>
					{/each}

					{@render node('internet', 'Internet', 'people, mail servers, bots', 'good')}
					{@render node('cf', 'Cloudflare edge', 'proxy · load balancer · DNS', net.cloudflare?.configured ? (net.cloudflare.status ?? 'waiting') : 'off')}
					{@render node('direct', 'Direct to hosts', 'DNS-only: mail', mailWorst)}
					{@render node('alerts', 'Alert channels', [...new Set(net.channels.map(chanWord))].join(' · ') || 'none set up', net.channels.length ? 'good' : 'warning')}
					{#each [...net.hosts, ...remotes] as h (h.name)}
						{@const b = boxes[h.name]}
						<g class="node" class:sel={selected === h.name} role="button" tabindex="0" aria-label="{h.name}: {h.word}" onclick={() => pick(h.name)} onkeydown={(e) => key(e, h.name)}>
							<rect class="box" x={b.x - b.w / 2} y={b.y - b.h / 2} width={b.w} height={b.h} rx="10" />
							{@render glyph(h.status, b.x - b.w / 2 + 12, b.y - b.h / 2 + 11)}
							<text x={b.x - b.w / 2 + 34} y={b.y - b.h / 2 + 23}>{h.name}</text>
							{#if h.mesh_ip}<text class="m" x={b.x + b.w / 2 - 12} y={b.y - b.h / 2 + 23} text-anchor="end">{h.mesh_ip}</text>{/if}
							<text class="s" x={b.x - b.w / 2 + 12} y={b.y - b.h / 2 + 42}>{h.roles || h.word}</text>
							<text class="s" x={b.x - b.w / 2 + 12} y={b.y - b.h / 2 + 62}>CPU {Math.round(h.cpu_pct)}% · mem {Math.round(h.mem_pct)}% · disk {Math.round(h.disk_pct)}%</text>
						</g>
					{/each}
				</svg>
				<div class="map-legend">
					<span><i class="ln" style="border-color:var(--s3)"></i>colour = layer when healthy</span>
					<span><i class="ln" style="border-color:var(--warning)"></i>warning</span>
					<span><i class="ln" style="border-color:var(--critical)"></i>critical</span>
					<span>thickness = traffic</span>
					{#if remotes.length}<span><i class="ln dash" style="border-color:var(--s3)"></i>a host's own link</span>{/if}
					<span>off-site probe: not set up</span>
				</div>
			</div>

			<aside class="map-panel" aria-live="polite">
				{#if selHost}
					<h2>{selHost.name}</h2>
					<div class="mt6"><Status s={sev(selHost.status)} word={selHost.word} /></div>
					{#if selHost.workstation}<p class="muted">A workstation: watched, but it never pages. Its problems stop at warning.</p>{/if}
					<dl class="kv">
						{#if selHost.mesh_ip}<dt>WireGuard</dt><dd class="mono">{selHost.mesh_ip}</dd>{/if}
						{#if selHost.roles}<dt>Runs</dt><dd>{selHost.roles}</dd>{/if}
						<dt>System</dt><dd>{selHost.os || '—'}</dd>
						<dt>Up for</dt><dd>{selHost.uptime_s ? duration(selHost.uptime_s) : '—'}</dd>
						<dt>Traffic</dt><dd>out {rate(selHost.tx_bps)} · in {rate(selHost.rx_bps)}</dd>
						<dt>Load</dt><dd>CPU {Math.round(selHost.cpu_pct)}% · mem {Math.round(selHost.mem_pct)}% · disk {Math.round(selHost.disk_pct)}%</dd>
						{#if selRemote}
							<dt>Link</dt><dd>{selRemote.link.name || 'its own link'} to {selRemote.link.via} · {ms(selRemote.link.ms)} · {selRemote.link.word}</dd>
						{:else}
							<dt>Mesh peers</dt><dd>{peers(selHost.name).map((p) => `${p.host} ${ms(p.ms)}`).join(' · ') || '—'}</dd>
							{#if ownLinks(selHost.name).length}<dt>Own links</dt><dd>{ownLinks(selHost.name).map((r) => `${r.name} ${ms(r.link.ms)}`).join(' · ')}</dd>{/if}
						{/if}
						{#if selHost.last_seen}<dt>Last report</dt><dd>{ago(selHost.last_seen)}</dd>{/if}
					</dl>
					<h3>Services</h3>
					<ul>
						{#each selHost.services as s (s.id)}
							<li>{@render iconLi(s.status)}<a href="/map/services">{s.name}</a><span class="muted">{s.status === 'good' ? 'OK' : s.status}</span></li>
						{:else}
							<li class="muted">Nothing from the service map runs here.</li>
						{/each}
					</ul>
					<a class="btn sm mt14" href="/hosts/{selHost.name}">Open host page <Icon name="arrow" size={14} /></a>
				{:else if selected === 'cf' && net.cloudflare}
					<h2>Cloudflare edge</h2>
					<div class="mt6"><Status s={sev(net.cloudflare.status ?? 'off')} word={net.cloudflare.word ?? 'Not set up'} /></div>
					<p class="muted">Proxies the websites and load-balances them across the hosts. Cairn OBS reads it through a read-only API token.</p>
					<dl class="kv">
						{#each net.cloudflare.pools ?? [] as p (p.name)}<dt>Pool</dt><dd>{p.name}: {p.up}/{p.total} origins healthy</dd>{/each}
						<dt>Zones</dt><dd>{net.cloudflare.zones ?? 0}</dd>
						{#if net.cloudflare.cloudflare_status}<dt>Cloudflare</dt><dd>{net.cloudflare.cloudflare_status.description}</dd>{/if}
					</dl>
					<a class="btn sm mt14" href="/cloudflare">Open Cloudflare page <Icon name="arrow" size={14} /></a>
				{:else if selected === 'direct'}
					<h2>Direct to hosts</h2>
					<div class="mt6"><Status s={sev(mailWorst)} word={mailWorst === 'good' ? 'All answering' : 'Trouble'} /></div>
					<p class="muted">Names that skip Cloudflare and go straight to a host: the mail servers.</p>
					<ul>
						{#each net.paths.filter((p) => p.kind === 'mail') as p (p.name)}
							<li>{@render iconLi(p.status)}<span class="mono">{p.name}</span><span class="muted">{p.host} · {p.word}</span></li>
						{/each}
					</ul>
					<a class="btn sm mt14" href="/checks">Open checks <Icon name="arrow" size={14} /></a>
				{:else if selected === 'alerts'}
					<h2>Alert channels</h2>
					<p class="muted">Where alerts go. Critical alerts always use at least one channel that works when your mail is down. Agents with watch_server set also alert if Cairn OBS itself goes quiet.</p>
					<ul>
						{#each net.channels as c (c.id)}
							<li>{@render iconLi('good')}<span>{chanWord(c)}</span><span class="muted">{c.min} and up</span></li>
						{/each}
					</ul>
					<a class="btn sm mt14" href="/alerts">Open alerts <Icon name="arrow" size={14} /></a>
				{:else}
					<h2>Internet</h2>
					<p class="muted">Where everyone comes from. It isn't watched from outside yet; an off-site probe is an option for later.</p>
				{/if}
			</aside>
		</div>
	</Panel>
{:else}
	<p class="empty">Loading…</p>
{/if}

<style>
	.map-wrap { display: grid; grid-template-columns: minmax(0, 1fr) 320px; }
	.map-canvas {
		position: relative; min-width: 0; border-right: 1px solid var(--border); overflow: hidden; min-height: 600px;
		background-image: radial-gradient(color-mix(in oklab, var(--muted-fg) 22%, transparent) 1px, transparent 1px); background-size: 22px 22px;
		padding: 54px 6px 52px;
	}
	.map-canvas svg { display: block; width: 100%; height: 100%; }
	.map-tools { position: absolute; left: 14px; top: 14px; right: 14px; display: flex; gap: 6px; flex-wrap: wrap; z-index: 2; }
	.chip { display: inline-flex; align-items: center; gap: 6px; height: 30px; padding: 0 12px; border-radius: 20px; border: 1px solid var(--border); background: var(--card); color: var(--muted-fg); font-size: 12px; cursor: pointer; }
	.chip.on { border-color: var(--primary); color: var(--foreground); background: var(--accent); }
	.chip .dot { width: 8px; height: 8px; border-radius: 50%; }
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
	.lnk { fill: none; stroke-linecap: round; }
	.lnk-l { fill: var(--muted-fg); font: 500 11px var(--mono); paint-order: stroke; stroke: var(--card); stroke-width: 4px; }
	.node { cursor: pointer; outline: none; }
	.node .box { fill: var(--card); stroke: var(--input); stroke-width: 1.2; }
	.node:hover .box, .node:focus-visible .box { stroke: var(--muted-fg); }
	.node.sel .box { stroke: var(--primary); stroke-width: 2; }
	.node text { fill: var(--foreground); font: 600 13px var(--sans); }
	.node text.s { fill: var(--muted-fg); font: 400 11px var(--sans); }
	.node text.m { fill: var(--muted-fg); font: 500 11px var(--mono); }
	.map-panel { padding: 20px; max-height: 720px; overflow-y: auto; }
	.map-panel h2 { font-size: 20px; }
	.map-panel h3 { font-size: 11px; font-weight: 600; letter-spacing: .14em; text-transform: uppercase; color: var(--muted-fg); margin-top: 20px; }
	.map-panel ul { list-style: none; margin: 8px 0 0; padding: 0; }
	.map-panel li { display: flex; align-items: center; gap: 8px; padding: 7px 0; border-bottom: 1px solid var(--border); font-size: 13px; }
	.map-panel li :global(svg) { width: 14px; height: 14px; flex: none; }
	.map-panel li .muted { margin-left: auto; font-size: 12px; text-align: right; }
	.muted { color: var(--muted-fg); }
	.map-panel p.muted { margin: 12px 0 0; }
	.kv { display: grid; grid-template-columns: 100px 1fr; gap: 10px 14px; margin: 14px 0 0; font-size: 13px; }
	.kv dt { color: var(--muted-fg); }
	.kv dd { margin: 0; }
	.mt6 { margin-top: 6px; }
	.mt14 { margin-top: 14px; }
	.sm { height: 30px; padding: 0 12px; font-size: 13px; }
	@media (max-width: 1100px) {
		.map-wrap { grid-template-columns: 1fr; }
		.map-canvas { border-right: 0; border-bottom: 1px solid var(--border); min-height: 0; }
		.map-panel { max-height: none; }
	}
	@media (max-width: 640px) {
		.map-canvas { padding: 92px 0 96px; }
	}
</style>
