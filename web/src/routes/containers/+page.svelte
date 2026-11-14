<script lang="ts">
	import Icon from '$lib/Icon.svelte';
	import Status from '$lib/Status.svelte';
	import Logs from '$lib/Logs.svelte';
	import Pager from '$lib/Pager.svelte';
	import { get, SignedOut, type Container, type ContainerSummary } from '$lib/api';
	import { live } from '$lib/live.svelte';

	// Filtered, sorted (problems first) and paged by obs-beacond: a fleet can
	// hold tens of thousands of containers.
	const limit = 200;
	let rows = $state<Container[]>([]);
	let sum = $state<ContainerSummary | null>(null);
	let total = $state(0);
	let counts = $state<Record<string, number>>({});
	let hosts = $state<string[]>([]);
	let error = $state('');
	let loaded = $state(false);
	let host = $state('');
	let show = $state<'running' | 'problems' | 'stopped' | 'all'>('all');
	let typed = $state('');
	let q = $state('');
	let offset = $state(0);

	$effect(() => {
		const t = typed;
		const d = setTimeout(() => { q = t.trim(); offset = 0; }, 250);
		return () => clearTimeout(d);
	});

	type List = { containers: Container[]; summary: ContainerSummary; total: number; counts: Record<string, number>; hosts: string[] };
	async function load(q: string, host: string, show: string, offset: number) {
		try {
			const p = new URLSearchParams({ q, host, show: show === 'all' ? '' : show, offset: String(offset), limit: String(limit) });
			const r = await get<List>('/api/v1/containers?' + p);
			rows = r.containers;
			sum = r.summary;
			total = r.total;
			counts = r.counts;
			hosts = r.hosts;
			error = '';
			loaded = true;
		} catch (e) {
			if (e instanceof SignedOut) void live.checkSession();
			else error = String(e);
		}
	}
	$effect(() => {
		const args = [q, host, show, offset] as const;
		void load(...args);
		const t = setInterval(() => void load(...args), 10000);
		return () => clearInterval(t);
	});

	const pick = (h: string) => { host = h; offset = 0; };
	const showing = (s: typeof show) => { show = s; offset = 0; };
	const image = (s: string) => s.split('/').pop() ?? s;
</script>

<svelte:head><title>Cairn OBS · Containers</title></svelte:head>

<div class="phead">
	<span class="tile"><Icon name="box" /></span>
	<div><h1>Containers</h1><p>Every container on every host, read from Docker through each agent's read-only helper. CI job containers are counted, not listed. What should run on a host is chosen under Configure.</p></div>
	{#if live.seesAll}<div class="acts"><a class="btn sm" href="/configure/hosts"><Icon name="settings" /> Configure</a></div>{/if}
</div>

{#if error}<p class="foot-note" style="color:var(--critical)">{error}</p>{/if}

<div class="filters">
	<Icon name="filter" />
	<input class="input" placeholder="Filter containers" aria-label="Filter containers" bind:value={typed} style="width:200px" />
	{#if hosts.length > 8}
		<!-- A chip per host stops working past a handful. -->
		<select class="input" aria-label="Host" value={host} onchange={(e) => pick(e.currentTarget.value)} style="width:auto">
			<option value="">All hosts</option>
			{#each hosts as h}<option value={h}>{h}</option>{/each}
		</select>
	{:else}
		<button class="chip" class:on={host === ''} onclick={() => pick('')}>All hosts</button>
		{#each hosts as h}<button class="chip" class:on={host === h} onclick={() => pick(h)}>{h}</button>{/each}
	{/if}
	<span style="width:8px"></span>
	<button class="chip" class:on={show === 'all'} onclick={() => showing('all')}>All <b>{counts.all ?? 0}</b></button>
	<button class="chip" class:on={show === 'running'} onclick={() => showing('running')}>Running fine <b>{counts.running ?? 0}</b></button>
	<button class="chip" class:on={show === 'problems'} onclick={() => showing('problems')}>Need a look <b>{counts.problems ?? 0}</b></button>
	<button class="chip" class:on={show === 'stopped'} onclick={() => showing('stopped')}>Stopped on purpose <b>{counts.stopped ?? 0}</b></button>
</div>

<div class="tbl-wrap">
	<table class="tbl">
		<thead><tr><th>State</th><th>Container</th><th>Host</th><th>Image</th><th>Docker says</th></tr></thead>
		<tbody>
			{#each rows as r (r.host + '/' + (r.id || r.name))}
				<tr>
					<td><Status s={r.verdict.severity} word={r.verdict.word} /></td>
					<td>{r.name}{#if r.project}<div class="sub">{r.project}</div>{/if}</td>
					<td class="sub">{r.host}</td>
					<td class="mono sub img" title={r.image}>{image(r.image)}</td>
					<td class="sub">{r.state === 'missing' ? r.verdict.detail : r.status}{#if r.logs?.length}<Logs lines={r.logs} />{/if}</td>
				</tr>
			{:else}
				<tr><td colspan="5" class="empty">{!loaded ? 'Loading…' : sum?.total ? 'No containers match.' : 'No containers reported yet.'}</td></tr>
			{/each}
		</tbody>
	</table>
</div>
<Pager bind:offset {limit} {total} what="containers" />
<p class="foot-note">{sum?.ci ? `${sum.ci} CI job containers running. ` : ''}Refreshes every 10 seconds.</p>

<style>
	.img { max-width: 280px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
</style>
