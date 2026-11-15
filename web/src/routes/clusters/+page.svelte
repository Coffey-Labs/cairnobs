<script lang="ts">
	import Icon from '$lib/Icon.svelte';
	import Panel from '$lib/Panel.svelte';
	import Status from '$lib/Status.svelte';
	import ArrayIO from '$lib/ArrayIO.svelte';
	import VaultLeases from '$lib/VaultLeases.svelte';
	import PartitionCPU from '$lib/PartitionCPU.svelte';
	import { get, SignedOut, type Cluster, type ClusterNode } from '$lib/api';
	import { live } from '$lib/live.svelte';
	import { ago } from '$lib/format';

	type Full = Cluster & { node_list: ClusterNode[] };
	let clusters = $state<Full[]>([]);
	let error = $state('');
	let loaded = $state(false);

	async function load() {
		try {
			clusters = (await get<{ clusters: Full[] }>('/api/v1/clusters?nodes=120')).clusters;
			error = '';
			loaded = true;
		} catch (e) {
			if (e instanceof SignedOut) void live.checkSession();
			else error = String(e);
		}
	}
	$effect(() => {
		void load();
		const t = setInterval(load, 10_000);
		return () => clearInterval(t);
	});

	const kinds = [['node', 'Nodes'], ['controller', 'Controllers'], ['pod', 'Pods'], ['vm', 'VMs'], ['pvc', 'Volumes'], ['job', 'Jobs'], ['cert', 'Certificates']] as const;
	const sevs = [['critical', 'critical'], ['warning', 'warning'], ['waiting', 'waiting'], ['off', 'stopped']] as const;
	const n = (c: Cluster, k: string, s?: string) => {
		const m = c.kinds[k] ?? {};
		return s ? (m[s] ?? 0) : Object.values(m).reduce((a, b) => a + b, 0);
	};
	const href = (c: string, extra: Record<string, string>) => '/workloads?' + new URLSearchParams({ source: c, ...extra });
</script>

<svelte:head><title>Cairn OBS · Clusters</title></svelte:head>

<div class="phead">
	<span class="tile"><Icon name="cluster" /></span>
	<div><h1>Clusters</h1><p>Each cluster a cluster agent watches: its nodes, and what runs on them by state. A node that's down is one problem, with what's on it listed under it.</p></div>
	<div class="acts"><a class="btn sm" href="/workloads?show=problems"><Icon name="grid" /> Workloads that need a look</a></div>
</div>

{#if error}<p class="foot-note" style="color:var(--critical)">{error}</p>{/if}
{#if loaded && clusters.length === 0}<p class="empty">No clusters yet. A cluster agent adds its cluster once it has joined.</p>{/if}

{#each clusters as c (c.name)}
	<div class="mt">
		<Panel title={c.title ? `${c.title} · read by ${c.read_by || c.name}` : c.name} aside={c.version ? `version ${c.version}` : ''}>
			<div class="top">
				<Status s={c.status} word={c.word} />
				<span class="sub">{c.heard ? `Heard ${ago(c.heard)}` : 'Never heard from'}{#if c.listed} · listed in full {ago(c.listed)}{/if}</span>
			</div>
			{#if c.parts?.length || c.operators}
				<ul class="parts" aria-label="The cluster's own parts">
					{#each c.parts ?? [] as p (p.name)}
						<li><Status s={p.severity} word={p.title} /><span class="sub">{p.word}</span></li>
					{/each}
					{#if c.operators}
						<li class="ops"><Status s={c.operators_ok === c.operators ? 'good' : 'warning'} word="Operators" /><span class="sub">{c.operators_ok} of {c.operators} available, not degraded or progressing</span></li>
					{/if}
				</ul>
			{/if}
			{#if c.partition}
				{@const p = c.partition}
				{@const cpu = c.usage ? (c.usage.cpu_pct ?? 0) : p.cpu_pct}
				<div class="nums">
					<div><b>{cpu != null ? Math.round(cpu) + '%' : '–'}</b><span>CPU, last minute{#if p.processing_units}{' '}of {p.processing_units} processing {p.processing_units === 1 ? 'unit' : 'units'}{/if}</span></div>
					<div><b class:hot={p.system_asp_pct >= 90}>{Math.round(p.system_asp_pct)}%</b><span>system ASP used</span></div>
					<a href={href(c.name, { show: 'problems' })} class:bad={c.problems > 0}><b>{c.problems}</b><span>{c.problems === 1 ? 'problem' : 'problems'}</span></a>
				</div>
				<p class="sub pline">{p.active_jobs.toLocaleString('en-US')} jobs active of {p.jobs.toLocaleString('en-US')} in the system{#if p.subsystems}{' · '}{p.subsystems_active} of {p.subsystems} subsystems active{/if}{#if p.interfaces}{' · '}{p.interfaces_active} of {p.interfaces} interfaces active{/if}{#if p.server}{' · on '}<a href={p.server.href} title={`${p.server.word}; partition ${p.server.partition} on the HMC`}>{p.server.name}</a>{#if p.model}{' '}(Power {p.model}){/if}{:else if p.model}{' · '}Power {p.model}{/if}</p>
				{#if c.history}<PartitionCPU source={c.name} history={c.history} />{/if}
			{:else}
			<div class="nums">
				<a href={href(c.name, { kind: 'node' })}><b>{c.nodes_ready}<small>/{c.nodes}</small></b><span>{(c.node_noun || 'Node').toLowerCase()}s ready</span></a>
				<a href={href(c.name, {})}><b>{c.workloads}</b><span>{c.noun === 'Array' ? 'volumes' : c.noun === 'HMC' ? 'partitions' : c.noun === 'Vault' || c.noun === 'OpenBao' ? 'certificates' : 'workloads'}</span></a>
				<a href={href(c.name, { show: 'problems' })} class:bad={c.problems > 0}><b>{c.problems}</b><span>{c.problems === 1 ? 'problem' : 'problems'}</span></a>
			</div>
			{/if}
			{#if c.usage?.io && c.history}<ArrayIO source={c.name} history={c.history} io={c.usage.io} />{/if}
			{#if c.usage?.leases != null && c.history}<VaultLeases source={c.name} history={c.history} leases={c.usage.leases} />{/if}
			{#if kinds.some(([k]) => n(c, k))}
			<div class="ktw"><table class="tbl kt">
				<thead><tr><th>Kind</th><th>All</th>{#each sevs as [, w]}<th>{w}</th>{/each}</tr></thead>
				<tbody>
					{#each kinds as [k, label]}
						{#if n(c, k)}
							<tr>
								<td><a href={href(c.name, { kind: k })}>{k === 'node' && c.node_noun ? c.node_noun + 's' : label}</a></td>
								<td class="mono">{n(c, k)}</td>
								{#each sevs as [s]}
									<td class="mono">{#if n(c, k, s)}<a class="sv {s}" href={href(c.name, { kind: k, show: s === 'critical' || s === 'warning' ? 'problems' : s })}>{n(c, k, s)}</a>{:else}<span class="sub">0</span>{/if}</td>
								{/each}
							</tr>
						{/if}
					{/each}
				</tbody>
			</table></div>
			{/if}
			{#if c.node_list.length}
				<div class="nodes" aria-label="Nodes, the ones that need a look first">
					{#each c.node_list as nd (nd.name)}
						<a class="node {nd.severity}" href={href(c.name, { node: nd.name })} title="{nd.name}: {nd.word}"><Status s={nd.severity} word={nd.name} /></a>
					{/each}
					{#if c.nodes > c.node_list.length}<a class="node more" href={href(c.name, { kind: 'node' })}>{c.nodes - c.node_list.length} more</a>{/if}
				</div>
			{/if}
		</Panel>
	</div>
{/each}
<p class="foot-note">Refreshes every 10 seconds.</p>

<style>
	.top { display: flex; align-items: center; gap: 12px; flex-wrap: wrap; margin-bottom: 14px; }
	.nums { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 12px; margin-bottom: 14px; }
	.nums a, .nums > div { display: flex; flex-direction: column; color: var(--foreground); }
	.nums b.hot { color: var(--warning); }
	.pline { margin: -6px 0 14px; font-size: 12px; }
	.nums b { font: 600 26px var(--mono); }
	.nums small { font-size: 14px; color: var(--muted-fg); }
	.nums span { color: var(--muted-fg); font-size: 12px; }
	.nums .bad b { color: var(--critical); }
	.ktw { margin-bottom: 14px; max-width: 100%; overflow-x: auto; }
	@media (max-width: 700px) { .kt th, .kt td { padding-left: 6px; padding-right: 6px; } .kt th { font-size: 10.5px; letter-spacing: 0.02em; } }
	.parts { list-style: none; margin: 0 0 14px; padding: 0; display: grid; gap: 6px; }
	.parts li { display: grid; grid-template-columns: minmax(180px, max-content) 1fr; gap: 12px; align-items: baseline; font-size: 13px; }
	@media (max-width: 700px) { .parts li { grid-template-columns: 1fr; gap: 2px; } }
	.kt th:not(:first-child), .kt td:not(:first-child) { text-align: right; }
	.sv.critical { color: var(--critical); font-weight: 600; }
	.sv.warning { color: var(--warning); font-weight: 600; }
	.nodes { display: flex; flex-wrap: wrap; gap: 6px; }
	.node { display: inline-flex; align-items: center; padding: 3px 8px; font-family: var(--mono); border: 1px solid var(--border); border-radius: 6px; font-size: 12px; color: var(--foreground); }
	.node.critical { border-color: color-mix(in oklab, var(--critical) 55%, transparent); }
	.node.warning { border-color: color-mix(in oklab, var(--warning) 55%, transparent); }
	.node.more { color: var(--muted-fg); }
	@media (max-width: 700px) { .nums { grid-template-columns: 1fr 1fr; } }
</style>
