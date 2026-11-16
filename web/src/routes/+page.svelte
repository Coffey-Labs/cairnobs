<script lang="ts">
	import Icon from '$lib/Icon.svelte';
	import Mark from '$lib/Mark.svelte';
	import Panel from '$lib/Panel.svelte';
	import Gauge from '$lib/Gauge.svelte';
	import Meter from '$lib/Meter.svelte';
	import Spark from '$lib/Spark.svelte';
	import Status from '$lib/Status.svelte';
	import { live } from '$lib/live.svelte';
	import { duration, hms, hour, rate, utc, zone } from '$lib/format';

	let now = $state(Date.now());
	$effect(() => {
		const t = setInterval(() => (now = Date.now()), 1000);
		return () => clearInterval(t);
	});

	const o = $derived(live.overview);
	const hosts = $derived(o?.hosts ?? []);
	// Totals over every host; a big fleet's hosts list is only the ones most in need.
	const total = $derived(o?.fleet?.hosts ?? hosts.length);
	const reporting = $derived(o?.fleet?.reporting ?? 0);
	const busiest = $derived(o?.fleet?.busiest);
	const fullest = $derived(o?.fleet?.fullest);
	const c = $derived(o?.containers);
	const crit = $derived(o?.attention.some((a) => a.severity === 'critical') ?? false);
	const hello = $derived(hour(now) < 12 ? 'Good morning' : hour(now) < 18 ? 'Good afternoon' : 'Good evening');
	const firstName = $derived((live.session?.user ?? '').split(' ')[0]);
</script>

<svelte:head><title>Cairn OBS · Dashboard</title></svelte:head>

<div class="greet">
	<div class="gridbg"></div>
	<div class="glow"></div>
	<div class="in">
		<div class="hello">
			<span class="mk"><Mark size={32} /></span>
			<div>
				<h1>{hello}{firstName ? ', ' + firstName : ''}</h1>
				<p>Here's how your systems are doing.</p>
			</div>
		</div>
		<div class="ctl">
			<span class="pillc">{reporting}/{total} hosts reporting</span>
			<div class="clock"><b>{hms(now)}</b><small>{zone(now)} · {utc(now)}</small></div>
		</div>
	</div>
</div>

{#if o}
	<section aria-labelledby="attn">
		<div class="attn-h">
			<span class="ping" class:crit class:clear={o.attention.length === 0}><i></i></span>
			<h2 id="attn">{o.attention.length ? `Needs attention · ${o.attention.length}` : 'All clear'}</h2>
			<span class="rule"></span>
		</div>
		{#if o.attention.length}
			<div class="attn">
				{#each o.attention.slice(0, 6) as a}
					<a class="acard {a.severity}" href={a.href}>
						<div class="body"><Status s={a.severity} /><div class="t">{a.title}</div>{#if a.detail}<p>{a.detail}</p>{/if}</div>
						<span class="arrow"><Icon name="arrow" /></span>
					</a>
				{/each}
			</div>
		{:else}
			<p class="foot-note clearline">Every host is reporting, no disk is past 80%, and every container is where it should be.</p>
		{/if}
	</section>

	<Panel title="Vital signs" aside="right now">
		<div class="gauges">
			<Gauge value={total ? reporting / total : 0} reading={reporting} unit="/{total}" label="Hosts reporting" detail={reporting === total ? 'all agents heard from' : `${total - reporting} silent or waiting`} tone={reporting === total ? 'data' : 'critical'} />
			<Gauge value={(busiest?.cpu_pct ?? 0) / 100} reading={Math.round(busiest?.cpu_pct ?? 0)} unit="%" label="Busiest CPU" detail={busiest?.host ?? '—'} bands={[{ from: 0.85, to: 1, tone: 'critical' }]} />
			<Gauge value={(fullest?.pct ?? 0) / 100} reading={Math.round(fullest?.pct ?? 0)} unit="%" label="Fullest disk" detail={fullest ? `${fullest.host} ${fullest.mount}` : '—'} tone={(fullest?.pct ?? 0) >= 90 ? 'critical' : (fullest?.pct ?? 0) >= 80 ? 'warning' : 'data'} bands={[{ from: 0.8, to: 0.9, tone: 'warning' }, { from: 0.9, to: 1, tone: 'critical' }]} />
			<Gauge value={c && c.running + c.problems ? c.running / (c.running + c.problems) : 0} reading={c?.running ?? 0} unit="/{(c?.running ?? 0) + (c?.problems ?? 0)}" label="Containers fine" detail={c?.problems ? `${c.problems} need a look` : 'all where they should be'} tone={c?.problems ? 'warning' : 'data'} />
		</div>
	</Panel>

	<div class="mt">
		<Panel title="Hosts" aside={total > hosts.length ? `the ${hosts.length} most in need, of ${total}` : 'cpu · last hour'} href={total > hosts.length ? '/hosts' : undefined} hrefLabel="All hosts">
			<div class="hostrow">
				{#each hosts as h (h.name)}
					<a class="hcard" href="/hosts/{h.name}">
						<div class="hh"><b>{h.name}</b><span class="os">{h.os ?? ''}</span><span class="st"><Status s={h.status} word={h.word} /></span></div>
						{#if h.last_seen}
							<div class="meters">
								<Meter label="CPU" value={h.cpu_pct} warn={85} crit={95} />
								<Meter label="Memory" value={h.mem_pct} warn={90} crit={95} />
								<Meter label="Fullest disk" value={h.fullest?.pct ?? 0} />
								<Meter label="Swap" value={h.swap_pct} warn={50} crit={80} />
							</div>
							<Spark data={h.history.cpu} max={100} label="{h.name} CPU, last hour" />
							<div class="foot"><span>OUT {rate(h.net.tx_bps)}</span><span>IN {rate(h.net.rx_bps)}</span><span>UP {duration(h.uptime_s)}</span></div>
						{:else}
							<p class="foot-note">No report yet. Is its agent running?</p>
						{/if}
					</a>
				{/each}
			</div>
		</Panel>
	</div>

	{#if o.clusters?.length}
		<div class="mt">
			<Panel title="Clusters" href="/clusters" hrefLabel="All clusters">
				<div class="clist">
					{#each o.clusters as k (k.name)}
						<a class="crow" href="/workloads?source={encodeURIComponent(k.name)}{k.problems ? '&show=problems' : ''}">
							<b class="mono">{k.name}</b>
							<Status s={k.status} word={k.word} />
							<span class="sub">{k.version ?? ''}</span>
							<span class="sub sum">{k.summary}</span>
						</a>
					{/each}
				</div>
			</Panel>
		</div>
	{/if}

	{#if c}
		<div class="mt">
			<Panel title="Containers" href="/containers" hrefLabel="All containers">
				<div class="csum">
					<div><b>{c.running}</b><span>running fine</span></div>
					<div class:bad={c.problems > 0}><b>{c.problems}</b><span>need a look</span></div>
					<div><b>{c.stopped}</b><span>stopped on purpose</span></div>
					<div><b>{c.ci}</b><span>CI jobs now</span></div>
				</div>
			</Panel>
		</div>
	{/if}
{:else}
	<p class="empty">Loading…</p>
{/if}

<style>
	.greet { position: relative; overflow: hidden; border: 1px solid var(--border); border-radius: 8px; background: var(--card); box-shadow: var(--shadow-soft); margin-bottom: 20px; }
	.gridbg { position: absolute; inset: 0; opacity: .6; pointer-events: none; background-image: linear-gradient(var(--border) 1px, transparent 1px), linear-gradient(90deg, var(--border) 1px, transparent 1px); background-size: 24px 24px; mask-image: linear-gradient(90deg, transparent, #000 45%, #000); }
	.glow { position: absolute; left: -96px; top: -96px; width: 256px; height: 256px; border-radius: 50%; background: color-mix(in oklab, var(--primary) 18%, transparent); filter: blur(48px); pointer-events: none; }
	.in { position: relative; display: flex; align-items: center; justify-content: space-between; gap: 16px; padding: 16px 20px; flex-wrap: wrap; }
	.hello { display: flex; align-items: center; gap: 16px; min-width: 0; }
	.mk { width: 48px; height: 48px; flex: none; border-radius: 8px; display: grid; place-items: center; background: color-mix(in oklab, var(--primary) 14%, transparent); }
	.greet h1 { font-size: 24px; }
	.greet p { margin: 0; color: var(--muted-fg); }
	.ctl { display: flex; align-items: center; gap: 14px; flex-wrap: wrap; }
	.pillc { padding: 5px 10px; border-radius: 20px; border: 1px solid color-mix(in oklab, var(--primary) 45%, transparent); color: var(--primary); font: 600 10px var(--mono); letter-spacing: .14em; text-transform: uppercase; }
	.clock { text-align: right; }
	.clock b { display: block; font: 600 22px var(--mono); line-height: 1.1; }
	.clock small { display: block; font: 500 10px var(--mono); letter-spacing: .14em; color: var(--muted-fg); text-transform: uppercase; }

	.attn-h { display: flex; align-items: center; gap: 12px; margin-bottom: 10px; }
	.attn-h h2 { font-size: 11px; font-weight: 600; letter-spacing: .16em; text-transform: uppercase; }
	.rule { height: 1px; flex: 1; background: linear-gradient(90deg, var(--border), transparent); }
	.ping { display: inline-flex; color: var(--warning); }
	.ping.crit { color: var(--critical); }
	.ping.clear { color: var(--good); }
	.ping i { position: relative; width: 8px; height: 8px; border-radius: 50%; background: currentColor; }
	.ping i::after { content: ''; position: absolute; inset: 0; border-radius: 50%; background: currentColor; animation: ping 1.6s cubic-bezier(0, 0, .2, 1) infinite; }
	.ping.clear i::after { animation: none; }
	@keyframes ping { 75%, 100% { transform: scale(2.4); opacity: 0; } }
	.attn { display: flex; flex-wrap: wrap; gap: 12px; margin-bottom: 20px; }
	.clearline { margin: 0 0 20px; }
	.acard { position: relative; flex: 1 1 260px; min-width: 0; display: flex; align-items: flex-start; gap: 12px; overflow: hidden; padding: 14px 16px 14px 20px; border-radius: var(--radius); border: 1px solid color-mix(in oklab, var(--warning) 50%, transparent); background: var(--card); box-shadow: var(--shadow-soft); color: var(--foreground); transition: transform .15s; }
	.acard:hover { transform: translateY(-2px); }
	.acard::before { content: ''; position: absolute; inset: 0 auto 0 0; width: 4px; background: var(--warning); }
	.acard.critical { border-color: color-mix(in oklab, var(--critical) 55%, transparent); }
	.acard.critical::before { background: var(--critical); }
	.acard .body { min-width: 0; flex: 1; }
	.acard .t { font-weight: 600; margin-top: 4px; }
	.acard p { margin: 4px 0 0; font-size: 12px; color: var(--muted-fg); }
	.arrow { color: var(--muted-fg); }

	.gauges { display: grid; grid-template-columns: repeat(auto-fit, minmax(150px, 1fr)); gap: 24px 16px; }
	.hostrow { display: grid; grid-template-columns: repeat(auto-fill, minmax(260px, 1fr)); gap: 12px; }
	.hcard { display: block; padding: 14px; border: 1px solid var(--border); border-radius: var(--radius); color: var(--foreground); background: color-mix(in oklab, var(--muted) 35%, var(--card)); }
	.hcard:hover { border-color: color-mix(in oklab, var(--primary) 45%, var(--border)); }
	.hh { display: flex; align-items: center; gap: 8px; margin-bottom: 12px; min-width: 0; }
	.hh b { font-size: 16px; }
	.os { color: var(--muted-fg); font-size: 12px; overflow: hidden; white-space: nowrap; text-overflow: ellipsis; }
	.hh .st { margin-left: auto; }
	.hcard :global(.spark) { margin-top: 12px; }
	.foot { display: flex; gap: 12px; margin-top: 8px; font: 500 11px var(--mono); color: var(--muted-fg); flex-wrap: wrap; }
	.csum { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 12px; }
	.csum div { display: flex; flex-direction: column; }
	.csum b { font: 600 26px var(--mono); }
	.csum span { color: var(--muted-fg); font-size: 12px; }
	.csum .bad b { color: var(--critical); }
	.clist { display: grid; gap: 6px; }
	.crow { display: grid; grid-template-columns: minmax(120px, 1.2fr) minmax(140px, 1.4fr) minmax(60px, .8fr) 2fr; align-items: center; gap: 12px; padding: 8px 10px; border: 1px solid var(--border); border-radius: var(--radius); color: var(--foreground); }
	.crow:hover { border-color: color-mix(in oklab, var(--primary) 45%, var(--border)); }
	@media (max-width: 700px) { .crow { grid-template-columns: 1fr 1fr; } .crow .sum { grid-column: 1 / -1; } }
	@media (max-width: 860px) { .clock { text-align: left; } .csum { grid-template-columns: 1fr 1fr; } }
</style>
