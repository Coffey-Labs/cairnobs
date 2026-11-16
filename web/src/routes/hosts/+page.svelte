<script lang="ts">
	import Icon from '$lib/Icon.svelte';
	import Status from '$lib/Status.svelte';
	import Pager from '$lib/Pager.svelte';
	import FleetGrid from '$lib/FleetGrid.svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { get, SignedOut, type HostRow } from '$lib/api';
	import { live } from '$lib/live.svelte';
	import { ago, pct } from '$lib/format';

	// Squares by site and role first (#72); the list, filtered and paged by
	// obs-beacond, a click away: a fleet can be thousands of hosts.
	const view = $derived(page.url.searchParams.get('view') === 'list' ? 'list' : 'grid');
	function setView(v: string) {
		const u = new URL(page.url);
		if (v === 'list') u.searchParams.set('view', 'list');
		else u.searchParams.delete('view');
		void goto(u, { replaceState: true, noScroll: true, keepFocus: true });
	}
	const limit = 100;
	let rows = $state<HostRow[]>([]);
	let total = $state(0);
	let counts = $state<Record<string, number>>({});
	let error = $state('');
	let loaded = $state(false);
	let q = $state('');
	let typed = $state('');
	let status = $state('');
	let offset = $state(0);

	// Search once typing pauses, from the first page.
	$effect(() => {
		const t = typed;
		const d = setTimeout(() => { q = t.trim(); offset = 0; }, 250);
		return () => clearTimeout(d);
	});

	async function load(q: string, status: string, offset: number) {
		try {
			const p = new URLSearchParams({ q, status, offset: String(offset), limit: String(limit) });
			const r = await get<{ hosts: HostRow[]; total: number; counts: Record<string, number> }>('/api/v1/hosts?' + p);
			rows = r.hosts;
			total = r.total;
			counts = r.counts;
			error = '';
			loaded = true;
		} catch (e) {
			if (e instanceof SignedOut) void live.checkSession();
			else error = String(e);
		}
	}
	$effect(() => {
		if (view !== 'list') return;
		const args = [q, status, offset] as const;
		void load(...args);
		const t = setInterval(() => void load(...args), 10_000);
		return () => clearInterval(t);
	});

	const choices = [
		['', 'All'],
		['problems', 'Need a look'],
		['critical', 'Critical'],
		['warning', 'Warning'],
		['waiting', 'Waiting'],
		['good', 'Fine']
	] as const;
	const count = (k: string) => (k === 'problems' ? (counts.critical ?? 0) + (counts.warning ?? 0) : (counts[k || 'all'] ?? 0));
</script>

<svelte:head><title>Cairn OBS · Hosts</title></svelte:head>

<div class="phead">
	<span class="tile"><Icon name="hosts" /></span>
	<div><h1>Hosts</h1><p>Every host with an agent, grouped by site and role so the odd one out stands out. Adding and removing hosts is under Configure.</p></div>
	<div class="acts">
		<span role="group" aria-label="Show as" class="views">
			<button class="chip" class:on={view === 'grid'} aria-pressed={view === 'grid'} onclick={() => setView('grid')}>By site</button>
			<button class="chip" class:on={view === 'list'} aria-pressed={view === 'list'} onclick={() => setView('list')}>List</button>
		</span>
		{#if live.seesAll}<a class="btn sm" href="/configure/hosts"><Icon name="settings" /> Configure</a>{/if}
	</div>
</div>

{#if view === 'grid'}
	<FleetGrid />
{:else}

{#if error}<p class="foot-note" style="color:var(--critical)">{error}</p>{/if}

<div class="filters">
	<Icon name="filter" />
	<input class="input" placeholder="Name or OS" aria-label="Filter hosts" bind:value={typed} style="width:200px" />
	{#each choices as [k, label]}
		<button class="chip" class:on={status === k} onclick={() => { status = k; offset = 0; }}>{label} <b>{count(k)}</b></button>
	{/each}
</div>

<div class="tbl-wrap">
	<table class="tbl">
		<thead><tr><th>State</th><th>Host</th><th>CPU</th><th>Memory</th><th>Fullest disk</th><th>Containers</th><th>Last report</th></tr></thead>
		<tbody>
			{#each rows as h (h.name)}
				<tr>
					<td><Status s={h.status} word={h.word} /></td>
					<td><a class="mono" href="/hosts/{encodeURIComponent(h.name)}">{h.name}</a>{#if h.os}<div class="sub">{h.os}</div>{/if}</td>
					<td class="mono">{h.last_seen ? pct(h.cpu_pct) : '—'}</td>
					<td class="mono">{h.last_seen ? pct(h.mem_pct) : '—'}</td>
					<td class="mono">{#if h.fullest}{pct(h.fullest.pct)} <span class="sub">{h.fullest.mount}</span>{:else}—{/if}</td>
					<td class="mono">{#if h.containers.total}{h.containers.running}{#if h.containers.problems}<span class="bad"> · {h.containers.problems} need a look</span>{/if}{:else}—{/if}</td>
					<td class="sub">{h.last_seen ? ago(h.last_seen) : 'never'}</td>
				</tr>
			{:else}
				<tr><td colspan="7" class="empty">{!loaded ? 'Loading…' : counts.all ? 'No hosts match.' : 'No hosts yet.'}</td></tr>
			{/each}
		</tbody>
	</table>
</div>
<Pager bind:offset {limit} {total} what="hosts" />
{/if}
<p class="foot-note">Refreshes every 10 seconds.</p>

<style>
	.bad { color: var(--critical); }
	.views { display: inline-flex; gap: 6px; }
</style>
