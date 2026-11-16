<script lang="ts">
	// Every service as rows, grouped by who depends on it (#71): its state in
	// words, its last 24 hours and its month, and for the one picked what
	// it's made of, what it needs and what breaks if it goes. The service
	// map's side panel as a page you can scan.
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import Icon from '$lib/Icon.svelte';
	import Status from '$lib/Status.svelte';
	import Strip from '$lib/Strip.svelte';
	import { get, type ServiceHistory, type Severity } from '$lib/api';

	interface Signal { kind: string; name: string; host?: string; status: Severity; word: string; href?: string }
	interface Service { id: string; name: string; tier: string; needs: string[]; needed_by: string[]; status: Severity; word: string; signals: Signal[]; history: ServiceHistory }

	let services = $state<Service[] | null>(null);
	let topology = $state(true);
	let error = $state('');
	async function load() {
		try {
			const r = await get<{ services: Service[]; topology: boolean }>('/api/v1/services');
			services = r.services;
			topology = r.topology;
			error = '';
		} catch (e) {
			error = String(e);
		}
	}
	$effect(() => {
		void load();
		const t = setInterval(() => void load(), 10000);
		return () => clearInterval(t);
	});

	const TIERS: [string, string, string][] = [
		['public', 'What customers use', 'when these break, people notice'],
		['apps', 'What runs it', 'the machinery behind those'],
		['data', 'Where the data lives', 'slow to fix, so watched closest'],
		['edge', 'At the edge', 'the outside services everything goes through']
	];
	const bad = (s: { status: Severity }) => s.status === 'warning' || s.status === 'serious' || s.status === 'critical';

	const show = $derived(page.url.searchParams.get('show') ?? '');
	const picked = $derived(page.url.searchParams.get('s') ?? '');
	const all = $derived(services ?? []);
	const troubled = $derived(all.filter(bad).length);
	const shown = $derived(all.filter((s) => (show === 'problems' ? bad(s) : show === 'working' ? !bad(s) : true)));
	const tiers = $derived.by(() => {
		const known = new Set(TIERS.map((t) => t[0]));
		const out = TIERS.map(([id, name, blurb]) => ({ id, name, blurb, rows: shown.filter((s) => s.tier === id) }));
		const rest = shown.filter((s) => !known.has(s.tier));
		if (rest.length) out.push({ id: 'other', name: 'Other services', blurb: '', rows: rest });
		return out.filter((t) => t.rows.length);
	});
	// The one picked, else the worst, else the first.
	const sel = $derived(
		all.find((s) => s.id === picked) ??
			[...all].sort((a, b) => rank(b.status) - rank(a.status))[0]
	);
	const byId = $derived(new Map(all.map((s) => [s.id, s])));
	function rank(s: Severity) {
		return ({ critical: 5, serious: 4, warning: 3, waiting: 2, off: 1, good: 0 } as Record<string, number>)[s] ?? 0;
	}

	function setParam(k: string, v: string) {
		const u = new URL(page.url);
		if (v) u.searchParams.set(k, v);
		else u.searchParams.delete(k);
		void goto(u, { replaceState: true, noScroll: true, keepFocus: true });
	}

	// What it's made of: the parts in trouble by name, the rest counted, so
	// a service with hundreds of containers stays readable.
	const parts = $derived.by(() => {
		if (!sel) return { trouble: [] as Signal[], fine: [] as Signal[], groups: [] as [string, number][] };
		const trouble = sel.signals.filter(bad);
		const fine = sel.signals.filter((g) => !bad(g));
		const counts = new Map<string, number>();
		for (const g of fine) counts.set(g.kind, (counts.get(g.kind) ?? 0) + 1);
		return { trouble, fine, groups: [...counts.entries()] };
	});
	const KIND: Record<string, [string, string]> = { check: ['check', 'checks'], container: ['container', 'containers'], unit: ['unit', 'units'], cloudflare: ['Cloudflare zone', 'Cloudflare zones'] };
	const kindWord = (k: string, n: number) => (KIND[k] ? KIND[k][n === 1 ? 0 : 1] : k);
	const upPct = (p: number) => (p < 0 ? 'no history yet' : p >= 99.995 ? '100%' : p.toFixed(2) + '%');
	const stateWord = (s: { status: Severity; word: string }) => (s.status === 'good' ? 'Working' : s.word);
	const heading = $derived.by(() => {
		if (!all.length) return '';
		const crit = all.filter((s) => s.status === 'critical').length;
		const look = troubled - crit;
		const ok = all.length - troubled;
		const bits = [`${ok} working`];
		if (look) bits.push(`${look} worth a look`);
		if (crit) bits.push(`${crit} down`);
		return `${all.length} services. ${bits.join(', ')}.`;
	});
</script>

<svelte:head><title>Cairn OBS · Services</title></svelte:head>

<div class="page">
	<div class="phead">
		<div class="tile"><Icon name="services" /></div>
		<div>
			<h1>{heading || 'Services'}</h1>
			<p>Grouped by who depends on them. Pick one to see what it’s made of and what breaks if it goes.</p>
		</div>
		{#if all.length}
			<div class="acts" role="group" aria-label="Show">
				<button class="chip" class:on={!show} aria-pressed={!show} onclick={() => setParam('show', '')}>All <b>{all.length}</b></button>
				<button class="chip" class:on={show === 'problems'} aria-pressed={show === 'problems'} onclick={() => setParam('show', 'problems')}>Needs a look <b>{troubled}</b></button>
				<button class="chip" class:on={show === 'working'} aria-pressed={show === 'working'} onclick={() => setParam('show', 'working')}>Working <b>{all.length - troubled}</b></button>
			</div>
		{/if}
	</div>

	{#if error}<p class="err" role="alert">{error}</p>{/if}

	{#if services === null}
		<p class="empty">Loading…</p>
	{:else if !topology || !all.length}
		<p class="empty">No services yet. Services come from the service map: declare them in topology.yaml, or correct what the map found (docs/maps.md).</p>
	{:else}
		<div class="split">
			<div class="list">
				{#each tiers as t (t.id)}
					<section aria-labelledby="tier-{t.id}">
						<h2 id="tier-{t.id}">{t.name}{#if t.blurb}<span> · {t.blurb}</span>{/if}</h2>
						<div class="box">
							{#each t.rows as s (s.id)}
								<button class="row" class:on={sel?.id === s.id} aria-pressed={sel?.id === s.id} onclick={() => setParam('s', s.id)}>
									<span class="st"><Status s={s.status} word={stateWord(s)} /></span>
									<span class="nm"><b>{s.name}</b><small>{s.needs.length ? 'needs ' + s.needs.map((n) => byId.get(n)?.name ?? n).join(', ') : `${s.signals.length} ${s.signals.length === 1 ? 'part' : 'parts'} watched`}</small></span>
									<span class="strip"><Strip cells={s.history.cells} name={s.name} /></span>
									<span class="pct mono" title="Up over 30 days">{upPct(s.history.pct_30d)}</span>
								</button>
							{/each}
						</div>
					</section>
				{/each}
			</div>

			{#if sel}
				<aside class="detail" aria-labelledby="detail-h">
					<div>
						<Status s={sel.status} word={stateWord(sel)} />
						<h2 id="detail-h">{sel.name}</h2>
					</div>
					<div class="blk">
						<h3>Made of</h3>
						{#if parts.trouble.length}
							<ul class="parts">
								{#each parts.trouble as g (g.kind + g.name + (g.host ?? ''))}
									<li><a href={g.href ?? '#'}><span class="mono">{g.name}</span>{#if g.host}<span class="sub"> on {g.host}</span>{/if}</a><Status s={g.status} word={g.word} /></li>
								{/each}
							</ul>
						{/if}
						{#if parts.groups.length}
							<p class="fine">{parts.trouble.length ? 'And ' : ''}{parts.groups.map(([k, n]) => `${n} ${kindWord(k, n)}`).join(', ')} {parts.trouble.length ? 'that are fine' : 'all fine'}.</p>
						{/if}
						{#if !sel.signals.length}<p class="fine">Nothing watched for it yet.</p>{/if}
					</div>
					<div class="two">
						<div class="blk">
							<h3>Needs</h3>
							{#if sel.needs.length}
								<ul class="links">{#each sel.needs as n (n)}{@const d = byId.get(n)}<li><button class="lnk" onclick={() => setParam('s', n)}>{d?.name ?? n}</button>{#if d && bad(d)} <Status s={d.status} word={d.word} />{/if}</li>{/each}</ul>
							{:else}<p class="fine">Nothing else.</p>{/if}
						</div>
						<div class="blk">
							<h3>If it breaks</h3>
							{#if sel.needed_by.length}
								<ul class="links">{#each sel.needed_by as n (n)}<li><button class="lnk" onclick={() => setParam('s', n)}>{byId.get(n)?.name ?? n}</button></li>{/each}</ul>
							{:else}<p class="fine">Nothing else depends on it.</p>{/if}
						</div>
					</div>
					<div class="blk">
						<h3>Uptime</h3>
						<p class="fine">{upPct(sel.history.pct_24h)} in the last day · {upPct(sel.history.pct_30d)} in 30 days</p>
						<Strip cells={sel.history.cells} name={sel.name} />
					</div>
					<div class="acts2">
						<a class="btn pri" href="/map/services?focus={encodeURIComponent('svc:' + sel.id)}">On the service map</a>
						<a class="btn" href="/reports">Reports</a>
					</div>
				</aside>
			{/if}
		</div>
	{/if}
</div>

<style>
	.err { color: var(--critical); }
	.split { display: flex; flex-wrap: wrap; gap: 24px; align-items: flex-start; }
	.list { flex: 999 1 620px; min-width: 0; display: flex; flex-direction: column; gap: 22px; }
	.list h2 { font-size: 16px; margin: 0 0 8px 4px; }
	.list h2 span { color: var(--muted-fg); font-weight: 400; }
	.box { background: var(--card); border: 1px solid var(--border); border-radius: 12px; overflow: hidden; box-shadow: var(--shadow-soft); }
	.row { display: grid; grid-template-columns: minmax(130px, 170px) minmax(0, 1fr) minmax(120px, 220px) 72px; gap: 16px; align-items: center; width: 100%; min-height: 64px; padding: 12px 18px; border: 0; border-top: 1px solid var(--border); background: transparent; color: var(--foreground); text-align: left; cursor: pointer; font-size: 14px; }
	.row:first-child { border-top: 0; }
	.row:hover { background: var(--muted); }
	.row.on { background: var(--accent); box-shadow: inset 0 0 0 1px var(--primary); }
	.st { min-width: 0; overflow: hidden; }
	.nm { display: flex; flex-direction: column; min-width: 0; }
	.nm b { font-size: 16px; }
	.nm small { color: var(--muted-fg); font-size: 13px; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
	.pct { text-align: right; color: var(--data); font-size: 13px; }

	.detail { flex: 1 1 340px; min-width: 0; position: sticky; top: 16px; background: var(--card); border: 1px solid var(--border); border-radius: 12px; padding: 22px; display: flex; flex-direction: column; gap: 18px; box-shadow: var(--shadow-soft); }
	.detail h2 { font-size: 24px; margin-top: 4px; }
	.blk h3 { font-size: 13px; font-weight: 600; color: var(--muted-fg); margin-bottom: 6px; }
	.parts { list-style: none; margin: 0 0 8px; padding: 0; display: flex; flex-direction: column; gap: 6px; }
	.parts li { display: flex; justify-content: space-between; gap: 10px; flex-wrap: wrap; padding: 8px 12px; background: var(--muted); border-radius: 8px; }
	.parts a { color: var(--foreground); min-width: 0; }
	.fine { margin: 0; color: color-mix(in oklab, var(--foreground) 85%, var(--muted-fg)); }
	.two { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 16px; }
	.links { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 4px; }
	.lnk { border: 0; background: none; padding: 0; color: var(--primary); cursor: pointer; font: inherit; text-align: left; }
	.lnk:hover { text-decoration: underline; }
	.detail :global(.strip) { margin-top: 8px; }
	.acts2 { display: flex; gap: 10px; flex-wrap: wrap; }

	@media (max-width: 760px) {
		.row { grid-template-columns: minmax(0, 1fr) auto; grid-template-areas: 'nm pct' 'st st' 'strip strip'; gap: 8px 12px; }
		.row .nm { grid-area: nm; } .row .pct { grid-area: pct; } .row .st { grid-area: st; } .row .strip { grid-area: strip; }
		.detail { position: static; }
	}
</style>
