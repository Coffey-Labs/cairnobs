<script lang="ts">
	import { get, type IO, type WorkloadHour } from '$lib/api';
	import Spark from '$lib/Spark.svelte';
	import { bytes } from '$lib/format';

	// An array's performance and space (docs/pure.md, step 4): now, and
	// hour by hour over a day, a month or a year, from obs-beacond's history.
	let { source, history, io }: { source: string; history: string; io: IO } = $props();

	const ranges = [[1, 'Day'], [30, '30 days'], [400, 'Year']] as const;
	let days = $state(1);
	let hours = $state<WorkloadHour[]>([]);
	// Hours change once an hour: read them when the range changes, and every ten minutes.
	$effect(() => {
		const q = new URLSearchParams({ source, workload: history, days: String(days) });
		const load = () => get<{ hours: WorkloadHour[] }>('/api/v1/workloads/hours?' + q).then((r) => (hours = r.hours)).catch(() => {});
		void load();
		const t = setInterval(load, 600_000);
		return () => clearInterval(t);
	});

	// At most 200 points a line: a year's hours are averaged in buckets.
	const series = (f: (h: WorkloadHour) => number) => {
		const v = hours.map(f);
		const per = Math.ceil(v.length / 200);
		if (per <= 1) return v;
		const out: number[] = [];
		for (let i = 0; i < v.length; i += per) {
			const b = v.slice(i, i + per);
			out.push(b.reduce((a, x) => a + x, 0) / b.length);
		}
		return out;
	};
	const peak = (f: (h: WorkloadHour) => number) => (hours.length ? Math.max(...hours.map(f)) : 0);
	const ops = (n: number) => (n >= 10_000 ? (n / 1000).toFixed(0) + 'k' : n >= 1000 ? (n / 1000).toFixed(1) + 'k' : n.toFixed(0));
	const lat = (us: number) => (us >= 1000 ? (us / 1000).toFixed(2) + ' ms' : us.toFixed(0) + ' µs');
	const pct = $derived(io.used_of ? Math.round((100 * io.used) / io.used_of) : 0);
</script>

<div class="io" aria-label="Performance and space">
	<div class="now">
		<div><b>{ops(io.read_ops)}<small>{' / '}{ops(io.write_ops)}</small></b><span>reads / writes a second</span></div>
		<div><b>{lat(io.read_us)}<small>{' / '}{lat(io.write_us)}</small></b><span>a read / a write</span></div>
		<div><b>{bytes(io.read_bps)}<small>{' / '}{bytes(io.write_bps)}</small></b><span>read / written a second</span></div>
		<div><b class:hot={pct >= 90}>{bytes(io.used)}{#if io.used_of}<small>{' of '}{bytes(io.used_of)}, {pct}%</small>{/if}</b><span>space used</span></div>
	</div>
	<div class="range">
		{#each ranges as [d, label]}<button class="chip" class:on={days === d} onclick={() => (days = d)}>{label}</button>{/each}
		<span class="sub">hourly averages{#if hours.length < 2}: none kept yet{/if}</span>
	</div>
	{#if hours.length > 1}
		<div class="lines">
			<div><span class="sub">Reads + writes a second · peak hour {ops(peak((h) => (h.read_ops ?? 0) + (h.write_ops ?? 0)))}</span><Spark data={series((h) => (h.read_ops ?? 0) + (h.write_ops ?? 0))} label="Reads and writes a second" /></div>
			<div><span class="sub">Write latency · worst {lat(peak((h) => h.write_us_max ?? 0))}</span><Spark data={series((h) => h.write_us ?? 0)} label="Write latency" /></div>
			<div><span class="sub">Read latency · worst {lat(peak((h) => h.read_us_max ?? 0))}</span><Spark data={series((h) => h.read_us ?? 0)} label="Read latency" /></div>
			<div><span class="sub">Bandwidth · peak hour {bytes(peak((h) => (h.read_bps ?? 0) + (h.write_bps ?? 0)))}/s</span><Spark data={series((h) => (h.read_bps ?? 0) + (h.write_bps ?? 0))} label="Bandwidth" /></div>
			<div><span class="sub">Space used</span><Spark data={series((h) => h.used ?? 0)} max={io.used_of ?? 0} label="Space used" /></div>
		</div>
	{/if}
</div>

<style>
	.io { margin-bottom: 14px; }
	.now { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 12px; margin-bottom: 10px; }
	.now div { display: flex; flex-direction: column; }
	.now b { font: 600 18px var(--mono); }
	.now b.hot { color: var(--warning); }
	.now small { font-size: 12px; color: var(--muted-fg); font-weight: 400; }
	.now span { color: var(--muted-fg); font-size: 12px; }
	.range { display: flex; gap: 6px; align-items: center; flex-wrap: wrap; margin-bottom: 8px; }
	.lines { display: grid; grid-template-columns: repeat(auto-fit, minmax(200px, 1fr)); gap: 8px 18px; }
	@media (max-width: 700px) { .now { grid-template-columns: 1fr 1fr; } }
</style>
