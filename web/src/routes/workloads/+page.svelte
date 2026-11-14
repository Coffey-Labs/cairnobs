<script lang="ts">
	import { page } from '$app/state';
	import { replaceState } from '$app/navigation';
	import Icon from '$lib/Icon.svelte';
	import Status from '$lib/Status.svelte';
	import Pager from '$lib/Pager.svelte';
	import { get, SignedOut, type Workload, type WorkloadHour } from '$lib/api';
	import Spark from '$lib/Spark.svelte';
	import { live } from '$lib/live.svelte';
	import { ago, bytes, sourceLabel } from '$lib/format';

	// Every node, pod, controller, job, volume and VM the cluster agents
	// send, filtered and paged by obs-beacond: at fleet size that's tens of
	// thousands of rows. The filters live in the address, so a link from
	// an incident lands on what it's about.
	const limit = 100;
	const u = page.url.searchParams;
	let source = $state(u.get('source') ?? '');
	let kind = $state(u.get('kind') ?? '');
	let ns = $state(u.get('ns') ?? '');
	let node = $state(u.get('node') ?? '');
	let owner = $state(u.get('owner') ?? '');
	let show = $state(u.get('show') ?? '');
	let typed = $state(u.get('q') ?? '');
	let q = $state(u.get('q') ?? '');
	let offset = $state(0);

	let rows = $state<Workload[]>([]);
	let total = $state(0);
	let counts = $state<Record<string, number>>({});
	let kinds = $state<Record<string, number>>({});
	let namespaces = $state<string[]>([]);
	let clusters = $state<string[]>([]);
	let hypervisors = $state<string[]>([]); // hosts reporting libvirt VMs
	let open = $state<string | null>(null);
	// The open row's last day, hour by hour, when it has usage.
	let hours = $state<{ key: string; list: WorkloadHour[] } | null>(null);
	$effect(() => {
		const w = rows.find((r) => r.source + '/' + r.id === open);
		if (!w || !w.usage) return;
		const key = open as string, workload = w.namespace ? w.namespace + '/' + w.name : w.name;
		if (hours?.key === key) return;
		get<{ hours: WorkloadHour[] }>(`/api/v1/workloads/hours?${new URLSearchParams({ source: w.source, workload })}`)
			.then((r) => { if (open === key) hours = { key, list: r.hours }; })
			.catch(() => {});
	});
	const cores = (n: number) => (n >= 10 ? n.toFixed(0) : n >= 1 ? n.toFixed(1) : n.toFixed(2));
	let error = $state('');
	let loaded = $state(false);

	$effect(() => {
		const t = typed;
		const d = setTimeout(() => { if (t.trim() !== q) { q = t.trim(); offset = 0; } }, 250);
		return () => clearTimeout(d);
	});

	const params = () => {
		const p = new URLSearchParams();
		for (const [k, v] of [['source', source], ['kind', kind], ['ns', ns], ['node', node], ['owner', owner], ['show', show], ['q', q]]) if (v) p.set(k, v);
		return p;
	};
	async function load(p: URLSearchParams, offset: number) {
		try {
			const qp = new URLSearchParams(p);
			qp.set('offset', String(offset));
			qp.set('limit', String(limit));
			const r = await get<{ workloads: Workload[]; total: number; counts: Record<string, number>; kinds: Record<string, number>; namespaces: string[]; clusters: string[]; hypervisors?: string[] }>('/api/v1/workloads?' + qp);
			rows = r.workloads;
			total = r.total;
			counts = r.counts;
			kinds = r.kinds;
			namespaces = r.namespaces;
			clusters = r.clusters;
			hypervisors = r.hypervisors ?? [];
			error = '';
			loaded = true;
		} catch (e) {
			if (e instanceof SignedOut) void live.checkSession();
			else error = String(e);
		}
	}
	$effect(() => {
		const p = params();
		const o = offset;
		try { replaceState('/workloads' + (p.size ? '?' + p : ''), {}); } catch { /* before the router is up */ }
		void load(p, o);
		const t = setInterval(() => void load(p, o), 10_000);
		return () => clearInterval(t);
	});

	const shows = [['', 'All'], ['problems', 'Need a look'], ['waiting', 'Waiting'], ['good', 'Fine'], ['off', 'Stopped']] as const;
	const showCount = (k: string) => counts[k || 'all'] ?? 0;
	const kindNames: Record<string, string> = { node: 'Nodes', pod: 'Pods', controller: 'Controllers', job: 'Jobs', pvc: 'Volumes', vm: 'VMs', service: 'Services', ingress: 'Ingresses', cert: 'Certificates' };
	const kindWord: Record<string, string> = { node: 'Node', pod: 'Pod', controller: 'Controller', job: 'Job', pvc: 'Volume', vm: 'VM', service: 'Service', ingress: 'Ingress', cert: 'Certificate' };
	const set = (fn: () => void) => { fn(); offset = 0; open = null; };
	const clear = () => set(() => { source = ''; kind = ''; ns = ''; node = ''; owner = ''; show = ''; typed = ''; q = ''; });
	const fact = (v: unknown) => (typeof v === 'object' ? JSON.stringify(v) : String(v));
</script>

<svelte:head><title>Cairn OBS · Workloads</title></svelte:head>

<div class="phead">
	<span class="tile"><Icon name="grid" /></span>
	<div><h1>Workloads</h1><p>Every node, pod, controller, job, volume and VM in your clusters and on your hosts, the ones that need a look first. Pods are judged with their controller.</p></div>
	<div class="acts"><a class="btn sm" href="/clusters"><Icon name="cluster" /> Clusters</a></div>
</div>

{#if error}<p class="foot-note" style="color:var(--critical)">{error}</p>{/if}

<div class="filters">
	<Icon name="filter" />
	<input class="input" placeholder="Name, namespace, owner or node" aria-label="Find a workload" bind:value={typed} style="width:240px" />
	{#if clusters.length + hypervisors.length > 1}
		<select class="input" aria-label="Cluster or host" value={source} onchange={(e) => set(() => { source = e.currentTarget.value; ns = ''; })}>
			<option value="">Every cluster and host</option>
			{#if hypervisors.length}
				<optgroup label="Clusters">{#each clusters as c}<option value={c}>{sourceLabel(c)}</option>{/each}</optgroup>
				<optgroup label="Hosts running VMs">{#each hypervisors as h}<option value={h}>{h}</option>{/each}</optgroup>
			{:else}
				{#each clusters as c}<option value={c}>{sourceLabel(c)}</option>{/each}
			{/if}
		</select>
	{/if}
	{#if namespaces.length}
		<select class="input" aria-label="Namespace" value={ns} onchange={(e) => set(() => (ns = e.currentTarget.value))}>
			<option value="">Every namespace</option>
			{#each namespaces as n}<option value={n}>{n}</option>{/each}
		</select>
	{/if}
	{#each shows as [k, label]}
		<button class="chip" class:on={show === k} onclick={() => set(() => (show = k))}>{label} <b>{showCount(k)}</b></button>
	{/each}
</div>
<div class="filters kinds">
	<button class="chip" class:on={kind === ''} onclick={() => set(() => (kind = ''))}>Every kind</button>
	{#each Object.keys(kindNames) as k}
		{#if kinds[k]}<button class="chip" class:on={kind === k} onclick={() => set(() => (kind = k))}>{kindNames[k]} <b>{kinds[k]}</b></button>{/if}
	{/each}
	{#if node}<button class="chip on" onclick={() => set(() => (node = ''))}>On {node} ×</button>{/if}
	{#if owner}<button class="chip on" onclick={() => set(() => (owner = ''))}>Of {owner} ×</button>{/if}
	{#if source && clusters.length + hypervisors.length <= 1}<button class="chip on" onclick={() => set(() => { source = ''; ns = ''; })}>In {sourceLabel(source)} ×</button>{/if}
	{#if source || kind || ns || node || owner || show || q}<button class="btn sm" onclick={clear}>Clear</button>{/if}
</div>

<div class="tbl-wrap">
	<table class="tbl">
		<thead><tr><th>State</th><th>Name</th><th>Kind</th><th>Using</th><th>Node</th><th>Where</th><th>Since</th></tr></thead>
		<tbody>
			{#each rows as w (w.source + '/' + w.id)}
				<tr class="row" class:sel={open === w.source + '/' + w.id} onclick={() => (open = open === w.source + '/' + w.id ? null : w.source + '/' + w.id)}>
					<td><Status s={w.severity} word={w.word} /></td>
					<td><span class="mono nm">{w.name}</span>{#if w.namespace}<div class="sub">{w.namespace}{#if w.owner} · {w.owner}{/if}</div>{/if}</td>
					<td class="sub">{kindWord[w.kind] ?? w.kind}</td>
					<td class="sub mono use">{#if w.usage}{cores(w.usage.cpu)} CPU{#if w.usage.cpu_pct}{' '}<b class:hot={w.usage.cpu_pct >= 90}>{Math.round(w.usage.cpu_pct)}%</b>{/if} · {bytes(w.usage.mem)}{#if w.usage.mem_pct}{' '}<b class:hot={w.usage.mem_pct >= 90}>{Math.round(w.usage.mem_pct)}%</b>{/if}{:else}—{/if}</td>
					<td class="mono sub">{#if w.node}<button class="link" onclick={(e) => { e.stopPropagation(); set(() => { node = w.node ?? ''; source = w.source; }); }}>{w.node}</button>{:else}—{/if}</td>
					<td class="sub">{#if hypervisors.includes(w.source)}<a href="/hosts/{w.source}">{w.source}</a>{:else}{sourceLabel(w.source)}{/if}</td>
					<td class="sub">{w.since ? ago(w.since) : '—'}</td>
				</tr>
				{#if open === w.source + '/' + w.id}
					<tr class="more"><td colspan="7">
						<dl>
							<dt>State</dt><dd>{w.word} <span class="sub">({w.state})</span></dd>
							{#if w.usage}
								{@const u = w.usage}
								{@const hs = hours?.key === w.source + '/' + w.id ? hours.list : []}
								<dt>CPU</dt><dd>{cores(u.cpu)} cores{#if u.cpu_of}{' '}of {cores(u.cpu_of)} ({Math.round(u.cpu_pct ?? 0)}%){:else}{' '}<span class="sub">(no limit)</span>{/if}</dd>
								<dt>Memory</dt><dd>{bytes(u.mem)}{#if u.mem_of}{' '}of {bytes(u.mem_of)} ({Math.round(u.mem_pct ?? 0)}%){:else}{' '}<span class="sub">(no limit)</span>{/if}</dd>
								<dt>Last day</dt><dd>{#if hs.length > 1}<div class="hours">
									<div><span class="sub">CPU, hourly average · peak {cores(Math.max(...hs.map((h) => h.cpu_max)))}</span><Spark data={hs.map((h) => h.cpu)} max={u.cpu_of ?? 0} label="CPU, last day" /></div>
									<div><span class="sub">Memory, hourly average · peak {bytes(Math.max(...hs.map((h) => h.mem_max)))}</span><Spark data={hs.map((h) => h.mem)} max={u.mem_of ?? 0} label="Memory, last day" /></div>
								</div>{:else}<span class="sub">Kept hourly from now on: back after the first hour or two.</span>{/if}</dd>
							{/if}
							{#if w.volumes?.length}<dt>On an array</dt><dd>{#each w.volumes as v, i}{#if i}, {/if}<button class="link mono" onclick={() => set(() => { source = v.source; ns = ''; q = v.name; typed = v.name; kind = ''; })}>{v.name}</button> on {v.array} <span class="sub">({v.word})</span>{/each}</dd>{/if}
							{#if w.pvcs?.length}<dt>Used by</dt><dd>{#each w.pvcs as p, i}{#if i}, {/if}<button class="link mono" onclick={() => set(() => { source = p.source; ns = p.namespace; q = p.name; typed = p.name; kind = ''; })}>{p.namespace}/{p.name}</button> in {sourceLabel(p.source)}{/each}</dd>{/if}
							{#if w.host}<dt>Host agent</dt><dd><a href={w.host.href}>{w.host.name}</a> <span class="sub">(the same machine, by its SMBIOS UUID)</span></dd>{/if}
							{#if w.owner}<dt>Belongs to</dt><dd><button class="link mono" onclick={() => set(() => { owner = w.owner ?? ''; source = w.source; ns = w.namespace ?? ''; kind = ''; })}>{w.owner}</button></dd>{/if}
							{#if w.kind === 'controller' || w.kind === 'node'}<dt>{w.kind === 'node' ? 'On it' : 'Its pods'}</dt><dd><button class="link" onclick={() => set(() => { if (w.kind === 'node') node = w.name; else { owner = w.name; ns = w.namespace ?? ''; } source = w.source; kind = 'pod'; })}>Show them</button></dd>{/if}
							<dt>Alerts</dt><dd>{w.kind === 'service' || w.kind === 'ingress' ? 'None: what it sends to is judged' : w.alert === 'page' ? 'Page (obs.cairnobs.org/alert)' : w.alert === 'off' ? 'Off (obs.cairnobs.org/alert)' : w.alert === 'warn' ? 'Warnings only (obs.cairnobs.org/alert)' : w.kind === 'node' ? 'Page' : 'Warnings only, until adopted or annotated'}</dd>
							{#each Object.entries(w.facts ?? {}) as [k, v]}<dt>{k.replaceAll('_', ' ')}</dt><dd class="mono">{fact(v)}</dd>{/each}
							{#each Object.entries(w.labels ?? {}) as [k, v]}<dt class="mono">{k}</dt><dd class="mono">{v}</dd>{/each}
							<dt>Id</dt><dd class="mono sub">{w.id}</dd>
						</dl>
					</td></tr>
				{/if}
			{:else}
				<tr><td colspan="7" class="empty">{!loaded ? 'Loading…' : counts.all ? 'Nothing matches.' : 'No workloads yet. A cluster agent sends them once it has joined.'}</td></tr>
			{/each}
		</tbody>
	</table>
</div>
<Pager bind:offset {limit} {total} what="workloads" />
<p class="foot-note">Refreshes every 10 seconds. Open a row for its details.</p>

<style>
	.kinds { margin-top: -6px; }
	.row { cursor: pointer; }
	.row.sel td { background: var(--accent); }
	.nm { font-weight: 600; }
	.link { background: none; border: 0; padding: 0; color: var(--primary); font: inherit; cursor: pointer; }
	.link:hover { text-decoration: underline; }
	.more td { background: color-mix(in oklab, var(--muted) 35%, var(--card)); }
	dl { display: grid; grid-template-columns: max-content 1fr; gap: 6px 16px; margin: 4px 0; font-size: 12.5px; }
	dt { color: var(--muted-fg); }
	dd { margin: 0; overflow-wrap: anywhere; }
	select.input { height: 34px; }
	.use { white-space: nowrap; }
	.use b { font-weight: 600; color: var(--foreground); }
	.use b.hot { color: var(--warning); }
	.hours { display: grid; grid-template-columns: repeat(auto-fit, minmax(200px, 1fr)); gap: 8px 18px; max-width: 640px; }
</style>
