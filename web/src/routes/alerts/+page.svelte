<script lang="ts">
	import Icon from '$lib/Icon.svelte';
	import Panel from '$lib/Panel.svelte';
	import Status from '$lib/Status.svelte';
	import Logs from '$lib/Logs.svelte';
	import { get, post, send, SignedOut, type Incident, type Incidents } from '$lib/api';
	import { live } from '$lib/live.svelte';
	import { ago, duration, hm, hmz, utc } from '$lib/format';

	let data = $state<Incidents | null>(null);
	let error = $state('');
	const admin = $derived(live.seesAll); // acknowledging, and a look at the setup

	async function load() {
		try {
			data = await get<Incidents>('/api/v1/incidents');
			error = '';
		} catch (e) {
			if (e instanceof SignedOut) void live.checkSession();
			else error = String(e);
		}
	}
	$effect(() => {
		void load();
		const t = setInterval(() => void load(), 5000);
		return () => clearInterval(t);
	});

	async function ack(i: Incident) {
		try {
			await post(`/api/v1/incidents/${encodeURIComponent(i.id)}/ack`);
			await load();
			void live.refresh();
		} catch (e) {
			error = String(e);
		}
	}
	// A learned dependency that grouped alerts, rejected here: the next
	// evaluation splits them, and the map never draws it again.
	let rejecting = $state('');
	async function reject(g: NonNullable<Incident['grouped']>[number]) {
		rejecting = g.from + g.to + (g.port ?? 0);
		try {
			await send('/api/v1/map/corrections', 'POST', { op: 'edge', from: g.from, to: g.to, port: g.port ?? 0, verdict: 'reject' });
			await load();
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		} finally {
			rejecting = '';
		}
	}
	const day = (s: string) => new Date(s).toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
	const lasted = (i: Incident) => duration((new Date(i.resolved ?? Date.now()).getTime() - new Date(i.opened).getTime()) / 1000);
</script>

<svelte:head><title>Cairn OBS · Alerts</title></svelte:head>

<div class="phead">
	<span class="tile"><Icon name="bell" /></span>
	<div>
		<h1>Alerts</h1>
		<p>Problems become alerts once they've lasted {data ? duration(data.for_s) : '2 min'}. Critical ones repeat every {data ? duration(data.repeat_s) : '15 min'} until someone acknowledges them.</p>
	</div>
	{#if admin}<div class="acts"><a class="btn sm" href="/configure/alerts"><Icon name="settings" /> Configure</a></div>{/if}
</div>

{#if error}<p class="foot-note" style="color:var(--critical)">{error}</p>{/if}

{#if data}
	<Panel title="Open" aside="{data.open.length} open" flush>
		{#each data.open as i (i.id)}
			<div class="inc">
				<span class="bar {i.severity}"></span>
				<div class="body">
					<div class="top"><Status s={i.severity} /><span class="mono sub">{i.id}</span></div>
					<a class="t" href={i.href}>{i.title}</a>
					{#if i.detail}<div class="sub">{i.detail}</div>{/if}
					{#if i.grouped?.length}
						<ul class="why" aria-label="Grouped by these learned dependencies">
							{#each i.grouped as g (g.from + g.to + (g.port ?? 0))}
								<li>
									<span>Grouped because <b>{g.from_name}</b> has talked to <b>{g.to_name}</b>{g.port ? ` :${g.port}` : ''} since {day(g.since)}</span>
									{#if live.admin}<button class="btn sm ghost" disabled={rejecting !== ''} onclick={() => reject(g)}>Not a dependency</button>{/if}
								</li>
							{/each}
						</ul>
					{/if}
					<div class="sub">
						<span title={utc(i.opened)}>Since {hmz(i.opened)} · {ago(i.opened)}</span>
						{#if !i.fired} · <b>holding</b>: nobody is told unless it lasts {duration(data.for_s)}
						{:else if i.ack} · acknowledged by {i.ack.by} at {hm(i.ack.at)}
						{:else} · sent at {hm(i.fired)}, <b>not acknowledged</b>{/if}
						{#if i.map} · <a href={i.map}>On the map</a>{/if}
					</div>
					{#if i.logs?.length}<Logs lines={i.logs} />{/if}
				</div>
				{#if i.fired && !i.ack && admin}<button class="btn pri sm" onclick={() => ack(i)}>Acknowledge</button>{/if}
			</div>
		{:else}
			<p class="empty">Nothing open.</p>
		{/each}
	</Panel>

	<div class="grid mt" class:g2={data.maintenance?.length}>
		<Panel title="Recently resolved" aside="last {data.recent.length}" flush>
			<div class="tbl-wrap flat">
				<table class="tbl">
					<thead><tr><th>Resolved</th><th>Incident</th><th class="r">Lasted</th></tr></thead>
					<tbody>
						{#each data.recent as i (i.id)}
							<tr>
								<td class="sub" title={utc(i.resolved ?? i.opened)}>{hmz(i.resolved ?? i.opened)}</td>
								<td><Status s={i.severity} word={i.title} />{#if i.ack}<div class="sub">acknowledged by {i.ack.by}</div>{/if}</td>
								<td class="r mono">{lasted(i)}</td>
							</tr>
						{:else}
							<tr><td colspan="3" class="empty">Nothing resolved yet.</td></tr>
						{/each}
					</tbody>
				</table>
			</div>
		</Panel>
		{#if data.maintenance?.length}
			<Panel title="Maintenance windows" aside="alerts for these hosts are held while one is open" flush>
				<div class="tbl-wrap flat">
					<table class="tbl">
						<thead><tr><th>Window</th><th>Hosts</th><th class="r">State</th></tr></thead>
						<tbody>
							{#each data.maintenance as w (w.name)}
								<tr>
									<td>{w.name}<div class="sub">{w.daily} {w.timezone} for {duration(w.duration_s)}</div></td>
									<td class="mono sub">{w.hosts.join(', ')}</td>
									<td class="r">{#if w.active}<Status s="warning" word="Open now" />{:else}<span class="sub" title={utc(w.next)}>opens in {duration((new Date(w.next).getTime() - Date.now()) / 1000)}</span>{/if}</td>
								</tr>
							{/each}
						</tbody>
					</table>
				</div>
			</Panel>
		{/if}
	</div>
	<p class="foot-note">{#if data.channels.length}Alerts go to {data.channels.map((c) => c.id).join(', ')}.{:else}No alert channels are set up, so alerts only show here.{/if}{#if admin}{' '}Channels and maintenance windows are set under <a href="/configure/alerts">Configure</a>.{/if}</p>
	{#if !data.stored}<p class="foot-note">obs-beacond has no database configured, so incidents are kept in memory: restarting obs-beacond forgets them, and anything still wrong alerts again after {duration(data.for_s)}.</p>{/if}
{:else}
	<p class="empty">Loading…</p>
{/if}

<style>
	.inc { display: flex; gap: 14px; padding: 16px 20px; border-top: 1px solid var(--border); align-items: flex-start; }
	.inc .bar { width: 4px; align-self: stretch; border-radius: 4px; background: var(--warning); flex: none; }
	.inc .bar.critical { background: var(--critical); }
	.inc .body { flex: 1; min-width: 0; }
	.top { display: flex; gap: 10px; align-items: center; }
	.t { display: block; color: var(--foreground); font-size: 16px; font-weight: 600; margin: 4px 0 2px; }
	.why { list-style: none; margin: 8px 0; padding: 0; display: grid; gap: 6px; }
	.why li { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; font-size: 13px; color: var(--muted-fg); padding: 6px 10px; border: 1px dashed var(--border); border-radius: 6px; }
	.why li b { color: var(--foreground); font-weight: 600; }
	.why li .btn { margin-left: auto; height: 26px; }
	.sm { height: 30px; padding: 0 12px; font-size: 13px; }
	.flat { border: 0; border-radius: 0; }
	:global(.panel) .empty { padding: 20px; }
</style>
