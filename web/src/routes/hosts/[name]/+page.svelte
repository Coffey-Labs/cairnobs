<script lang="ts">
	import { page } from '$app/state';
	import Icon from '$lib/Icon.svelte';
	import Panel from '$lib/Panel.svelte';
	import Spark from '$lib/Spark.svelte';
	import Status from '$lib/Status.svelte';
	import { get, SignedOut, type Attention, type Container, type Host } from '$lib/api';
	import { live } from '$lib/live.svelte';
	import { ago, bytes, duration, hmz, rate } from '$lib/format';

	let host = $state<Host | null>(null);
	let containers = $state<Container[]>([]);
	let attention = $state<Attention[]>([]);
	let error = $state('');
	const name = $derived(page.params.name ?? '');
	// Windows has no load average, and its services aren't systemd's (docs/windows.md).
	const windows = $derived(host?.os?.startsWith('Windows') ?? false);

	async function load(n: string) {
		try {
			const r = await get<{ host: Host; containers: Container[]; attention: Attention[] }>('/api/v1/hosts/' + encodeURIComponent(n));
			host = r.host;
			containers = r.containers;
			attention = r.attention ?? [];
			error = '';
		} catch (e) {
			if (e instanceof SignedOut) void live.checkSession();
			else error = String(e);
		}
	}
	$effect(() => {
		const n = name;
		host = null;
		void load(n);
		const t = setInterval(() => void load(n), 5000);
		return () => clearInterval(t);
	});
	const rank: Record<string, number> = { critical: 0, warning: 1, good: 2, off: 3 };
	const sorted = $derived([...containers].sort((a, b) => (rank[a.verdict.severity] ?? 9) - (rank[b.verdict.severity] ?? 9) || a.name.localeCompare(b.name)));
	// A Windows server without Docker has no containers to show.
	const showContainers = $derived(!windows || sorted.length > 0 || !!host?.containers_error);
</script>

<svelte:head><title>Cairn OBS · {name}</title></svelte:head>

<div class="phead">
	<span class="tile"><Icon name="hosts" /></span>
	<div>
		<h1>{name}</h1>
		<p>{#if host?.os}{host.os} · {windows ? 'build' : 'kernel'} {host.kernel} · {host.cpus} CPUs · {bytes(host.mem_total)} memory{:else if host}Waiting for this host's first report.{/if}</p>
		{#if host?.vm}
			<p class="vm">VM <a href={host.vm.href}>{host.vm.namespace ? host.vm.namespace + '/' : ''}{host.vm.name}</a> {#if host.vm.on_host}on <a href="/hosts/{host.vm.source}">{host.vm.source}</a>{:else}in {host.vm.title ?? host.vm.source}{/if}{#if host.vm.node}, on {host.vm.node}{/if} · <span class:bad={!['ok', 'updating', 'degraded'].includes(host.vm.state)}>{host.vm.word}</span></p>
		{/if}
		{#if host?.vms}
			<p class="vm">{#if host.vms.error}<span class="bad">Can't read its VMs</span>: {host.vms.error}{:else}Runs <a href={host.vms.href}>{host.vms.total} {host.vms.total === 1 ? 'VM' : 'VMs'}</a>, {host.vms.running} running{/if}</p>
		{/if}
	</div>
	<div class="acts">
		{#if host}<Status s={host.status} word={host.word} />{/if}
		{#if host && live.seesAll}<a class="btn sm" href="/configure/hosts/{encodeURIComponent(name)}"><Icon name="settings" /> Configure</a>{/if}
	</div>
</div>

{#if error}<p class="foot-note" style="color:var(--critical)">{error}</p>{/if}

{#if host?.last_seen}
	{#if attention.length}
		<ul class="attn">{#each attention as a}<li><Status s={a.severity} /> {a.title}{#if a.detail}<span class="sub"> · {a.detail}</span>{/if}</li>{/each}</ul>
	{/if}
	<div class="grid g4">
		<Panel title="CPU"><div class="stat"><div class="val">{Math.round(host.cpu_pct)}<small>%</small></div><div class="sub">{#if windows}{host.cpus} CPUs{:else}load {host.load.map((l) => l.toFixed(2)).join(' · ')}{/if}</div></div><Spark data={host.history.cpu} max={100} label="CPU, last hour" /></Panel>
		<Panel title="Memory"><div class="stat"><div class="val">{Math.round(host.mem_pct)}<small>%</small></div><div class="sub">of {bytes(host.mem_total)} · swap {Math.round(host.swap_pct)}%</div></div><Spark data={host.history.mem} max={100} label="Memory, last hour" /></Panel>
		<Panel title="Network out"><div class="stat"><div class="val">{rate(host.net.tx_bps)}</div><div class="sub">in {rate(host.net.rx_bps)}</div></div><Spark data={host.history.tx} label="Network out, last hour" /></Panel>
		<Panel title="Up for"><div class="stat"><div class="val">{duration(host.uptime_s)}</div><div class="sub" title={hmz(host.last_seen ?? Date.now())}>last report {ago(host.last_seen)} · agent {host.agent}</div></div></Panel>
	</div>

	<div class="grid mt" class:g2={showContainers}>
		<Panel title="Disks" aside="{host.disks.length} filesystems" flush>
			<div class="tbl-wrap flat">
				<table class="tbl">
					<thead><tr><th>Mount</th><th>Type</th><th class="r">Used</th><th class="r">Size</th><th>Full</th></tr></thead>
					<tbody>
						{#each host.disks as d}
							<tr>
								<td class="mono">{d.mount}</td><td class="sub">{d.fs}</td>
								<td class="r mono">{bytes(d.used)}</td><td class="r mono">{bytes(d.size)}</td>
								<td><div class="fill" class:warn={d.pct >= 80 && d.pct < 90} class:crit={d.pct >= 90}><i style="width:{d.pct}%"></i></div> <span class="mono sub">{Math.round(d.pct)}%</span></td>
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
		</Panel>
		{#if showContainers}<Panel title="Containers" aside="{host.containers.running} fine · {host.containers.problems} need a look" flush>
			{#if host.containers_error}<p class="foot-note pad">Can't read containers: {host.containers_error}</p>{/if}
			<div class="tbl-wrap flat">
				<table class="tbl">
					<thead><tr><th>State</th><th>Container</th><th>Docker says</th></tr></thead>
					<tbody>
						{#each sorted as c (c.id || c.name)}
							<tr><td><Status s={c.verdict.severity} word={c.verdict.word} /></td><td>{c.name}</td><td class="sub">{c.state === 'missing' ? c.verdict.detail : c.status}</td></tr>
						{:else}
							<tr><td colspan="3" class="empty">No containers on this host.</td></tr>
						{/each}
					</tbody>
				</table>
			</div>
		</Panel>{/if}
	</div>
	{#if host.units?.length}
		<div class="mt">
			<Panel title="System services" aside={windows ? 'Windows services Cairn OBS expects here' : 'systemd units Cairn OBS expects here'} flush>
				<div class="tbl-wrap flat">
					<table class="tbl">
						<thead><tr><th>State</th><th>{windows ? 'Service' : 'Unit'}</th><th>{windows ? 'Windows says' : 'systemd says'}</th>{#if !windows}<th class="r">Restarts</th>{/if}<th class="r">Running for</th></tr></thead>
						<tbody>
							{#each host.units as u (u.name)}
								<tr>
									<td><Status s={u.severity} word={u.word} /></td>
									<td class="mono">{windows ? u.name.replace(/\.service$/, '') : u.name}</td>
									<td class="sub">{u.active ? `${u.active} (${u.sub})` : '—'}</td>
									{#if !windows}<td class="r mono">{u.restarts ?? 0}</td>{/if}
									<td class="r sub">{u.since && u.active === 'active' ? duration((Date.now() - new Date(u.since).getTime()) / 1000) : '—'}</td>
								</tr>
							{/each}
						</tbody>
					</table>
				</div>
			</Panel>
		</div>
	{/if}
	{#if host.events}
		<div class="mt" id="events">
			<Panel title="Event log" aside="Errors and criticals in System and Application, the last day" flush>
				{#if host.events.error}<p class="pad sub">Couldn't read: {host.events.error}</p>{/if}
				<div class="tbl-wrap flat">
					<table class="tbl">
						<thead><tr><th>Level</th><th>Source</th><th class="r">Event</th><th class="r">Times</th><th>Latest</th><th>What it said</th></tr></thead>
						<tbody>
							{#each host.events.list as g (g.log + g.source + g.id)}
								<tr>
									<td><Status s={g.level === 'critical' ? 'critical' : 'warning'} word={g.level === 'critical' ? 'Critical' : 'Error'} /></td>
									<td><span class="mono">{g.source}</span><div class="sub">{g.log}</div></td>
									<td class="r mono">{g.id}</td>
									<td class="r mono">{g.count}</td>
									<td class="sub nowrap">{ago(g.last)}</td>
									<td class="sub msg">{g.message || '—'}</td>
								</tr>
							{:else}
								<tr><td colspan="6" class="empty">Nothing in the last day.</td></tr>
							{/each}
						</tbody>
					</table>
				</div>
			</Panel>
		</div>
	{/if}
{:else if host}
	<p class="empty">No report from {name} yet. Is its agent running and pointed at this obs-beacond?</p>
{/if}

<style>
	.attn { list-style: none; padding: 0; margin: 0 0 16px; display: grid; gap: 6px; }
	.attn li { font-size: 13px; }
	.flat { border: 0; border-radius: 0; }
	.pad { padding: 0 20px; }
	.fill { display: inline-block; vertical-align: middle; width: 80px; height: 6px; border-radius: 3px; background: color-mix(in oklab, var(--data) 16%, transparent); overflow: hidden; }
	.fill i { display: block; height: 100%; background: var(--data); }
	.fill.warn i { background: var(--warning); }
	.fill.crit i { background: var(--critical); }
	.stat :global(.spark), :global(.panel) :global(.spark) { margin-top: 10px; }
	.vm { margin-top: 2px; }
	.msg { max-width: 60ch; min-width: 24ch; }
	.nowrap { white-space: nowrap; }
	.vm .bad { color: var(--critical); font-weight: 500; }
</style>
