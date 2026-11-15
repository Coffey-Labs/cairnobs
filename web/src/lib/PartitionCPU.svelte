<script lang="ts">
	import { get, type WorkloadHour } from '$lib/api';
	import Spark from '$lib/Spark.svelte';

	// An IBM i partition's CPU (docs/ibmi.md): hour by hour over a day, a
	// month or a year, from obs-beacond's history, as a share of the
	// processing units it may use.
	let { source, history }: { source: string; history: string } = $props();

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

	const pct = (h: WorkloadHour, v: number) => (h.cpu_of ? (100 * v) / h.cpu_of : 0);
	// At most 200 points: a year's hours are averaged in buckets.
	const series = $derived.by(() => {
		const v = hours.map((h) => pct(h, h.cpu));
		const per = Math.ceil(v.length / 200);
		if (per <= 1) return v;
		const out: number[] = [];
		for (let i = 0; i < v.length; i += per) {
			const b = v.slice(i, i + per);
			out.push(b.reduce((a, x) => a + x, 0) / b.length);
		}
		return out;
	});
	const peak = $derived(hours.length ? Math.max(...hours.map((h) => pct(h, h.cpu_max))) : 0);
</script>

<div class="cpu" aria-label="CPU">
	<div class="range">
		{#each ranges as [d, label]}<button class="chip" class:on={days === d} onclick={() => (days = d)}>{label}</button>{/each}
		<span class="sub">CPU, hourly averages{#if hours.length < 2}: none kept yet{:else} · peak {Math.round(peak)}%{/if}</span>
	</div>
	{#if hours.length > 1}<Spark data={series} max={100} label="CPU" />{/if}
</div>

<style>
	.cpu { margin-bottom: 14px; }
	.range { display: flex; gap: 6px; align-items: center; flex-wrap: wrap; margin-bottom: 8px; }
</style>
