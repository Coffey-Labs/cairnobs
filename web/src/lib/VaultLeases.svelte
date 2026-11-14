<script lang="ts">
	import { get, type WorkloadHour } from '$lib/api';
	import Spark from '$lib/Spark.svelte';

	// A Vault cluster's lease count (docs/vault.md, step 3): now, and hour
	// by hour over a day, a month or a year, from obs-beacond's history.
	let { source, history, leases }: { source: string; history: string; leases: number } = $props();

	const ranges = [[1, 'Day'], [30, '30 days'], [400, 'Year']] as const;
	let days = $state(1);
	let hours = $state<WorkloadHour[]>([]);
	$effect(() => {
		const q = new URLSearchParams({ source, workload: history, days: String(days) });
		const load = () => get<{ hours: WorkloadHour[] }>('/api/v1/workloads/hours?' + q).then((r) => (hours = r.hours)).catch(() => {});
		void load();
		const t = setInterval(load, 600_000);
		return () => clearInterval(t);
	});

	// At most 200 points: a year's hours are averaged in buckets.
	const series = $derived.by(() => {
		const v = hours.map((h) => h.leases ?? 0);
		const per = Math.ceil(v.length / 200);
		if (per <= 1) return v;
		const out: number[] = [];
		for (let i = 0; i < v.length; i += per) {
			const b = v.slice(i, i + per);
			out.push(b.reduce((a, x) => a + x, 0) / b.length);
		}
		return out;
	});
	const peak = $derived(hours.length ? Math.max(...hours.map((h) => h.leases_max ?? 0)) : 0);
	const n = (v: number) => Math.round(v).toLocaleString('en-US');
</script>

<div class="leases" aria-label="Leases">
	<div class="now"><b>{n(leases)}</b><span>leases now</span></div>
	<div class="range">
		{#each ranges as [d, label]}<button class="chip" class:on={days === d} onclick={() => (days = d)}>{label}</button>{/each}
		<span class="sub">hourly averages{#if hours.length < 2}: none kept yet{:else} · peak {n(peak)}{/if}</span>
	</div>
	{#if hours.length > 1}<Spark data={series} label="Leases" />{/if}
</div>

<style>
	.leases { margin-bottom: 14px; }
	.now { display: flex; align-items: baseline; gap: 8px; margin-bottom: 8px; }
	.now b { font: 600 18px var(--mono); }
	.now span { color: var(--muted-fg); font-size: 12px; }
	.range { display: flex; gap: 6px; align-items: center; flex-wrap: wrap; margin-bottom: 8px; }
</style>
