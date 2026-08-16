<script lang="ts">
	// Fixture data + a large-N stress case, per Phase 5 task 4's "test
	// with a fixture dataset large enough to expose rendering perf
	// issues" requirement -- not just a handful of rows. Render time is
	// measured and shown next to each large chart so the perf claim in
	// /docs/design-system.md is backed by a number produced here, not an
	// assumption.
	import { TimeSeriesChart, BarChart, TopN, Heatmap, SingleStat } from '$lib/charts';
	import { Card } from '$lib/components/ui';
	import type { QueryResult } from '$lib/api';

	const SERVICES = ['api', 'ingest', 'alerting', 'enterprise-auth', 'search', 'clickhouse'];
	const HOSTS = ['host-01', 'host-02', 'host-03', 'host-04', 'host-05', 'host-06', 'host-07', 'host-08'];

	function iso(ms: number): string {
		return new Date(ms).toISOString();
	}

	function realisticTimeSeries(): QueryResult {
		// 3 services, one point every 3 minutes over 24h -- ~480 points/series,
		// 1440 rows total, the shape a real "error count by service over the
		// default 24h dashboard range" panel actually returns.
		const now = Date.now();
		const rows: unknown[][] = [];
		for (const service of SERVICES.slice(0, 3)) {
			let base = 5 + Math.random() * 10;
			for (let i = 480; i >= 0; i--) {
				base = Math.max(0, base + (Math.random() - 0.5) * 3);
				rows.push([iso(now - i * 3 * 60_000), service, Math.round(base)]);
			}
		}
		return { columns: ['timestamp', 'service', 'count'], rows };
	}

	function largeTimeSeries(): QueryResult {
		// 6 series x 5000 points = 30,000 rows -- well past what a
		// realistic dashboard panel would ever show, deliberately, to
		// find the rendering ceiling rather than assume one.
		const now = Date.now();
		const rows: unknown[][] = [];
		for (const service of SERVICES) {
			let base = 20 + Math.random() * 30;
			for (let i = 5000; i >= 0; i--) {
				base = Math.max(0, base + (Math.random() - 0.5) * 4);
				rows.push([iso(now - i * 15_000), service, Math.round(base)]);
			}
		}
		return { columns: ['timestamp', 'service', 'count'], rows };
	}

	function stackedBar(): QueryResult {
		const rows: unknown[][] = [];
		for (const host of HOSTS) {
			for (const sev of ['INFO', 'WARN', 'ERROR', 'FATAL']) {
				const base = sev === 'INFO' ? 200 : sev === 'WARN' ? 40 : sev === 'ERROR' ? 12 : 2;
				rows.push([host, sev, Math.round(base + Math.random() * base * 0.6)]);
			}
		}
		return { columns: ['host', 'severity', 'count'], rows };
	}

	function topN(): QueryResult {
		const rows = SERVICES.map((s, i) => [s, 1200 - i * 180 + Math.round(Math.random() * 60)])
			.sort((a, b) => (b[1] as number) - (a[1] as number));
		return { columns: ['service', 'count'], rows };
	}

	function heatmapData(): QueryResult {
		// hour-of-day x day-of-week log volume -- the canonical "when do
		// we get paged" heatmap.
		const days = ['Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat', 'Sun'];
		const rows: unknown[][] = [];
		for (let h = 0; h < 24; h++) {
			for (const day of days) {
				const businessHours = h >= 9 && h <= 18 && day !== 'Sat' && day !== 'Sun';
				const base = businessHours ? 80 : 15;
				rows.push([String(h).padStart(2, '0'), day, Math.round(base + Math.random() * base * 0.8)]);
			}
		}
		return { columns: ['hour', 'day', 'count'], rows };
	}

	function singleStatSeries(): QueryResult {
		const now = Date.now();
		const rows: unknown[][] = [];
		let base = 12;
		for (let i = 60; i >= 0; i--) {
			base = Math.max(0, base + (Math.random() - 0.5) * 2);
			rows.push([iso(now - i * 60_000), Math.round(base * 10) / 10]);
		}
		return { columns: ['timestamp', 'p99_latency_ms'], rows };
	}

	function timeit<T>(fn: () => T): [T, number] {
		const start = performance.now();
		const result = fn();
		return [result, performance.now() - start];
	}

	const [realisticTS, realisticTSMs] = timeit(realisticTimeSeries);
	const [largeTS, largeTSGenMs] = timeit(largeTimeSeries);
	const [stacked] = timeit(stackedBar);
	const [ranked] = timeit(topN);
	const [heat] = timeit(heatmapData);
	const [statSeries] = timeit(singleStatSeries);

	let largeRenderMs = $state<number | null>(null);
	$effect(() => {
		// EChart's own render happens inside the component's effect, one
		// tick after mount -- measuring from here via rAF brackets the
		// actual paint, not just this page's synchronous data prep above.
		const start = performance.now();
		requestAnimationFrame(() => {
			requestAnimationFrame(() => {
				largeRenderMs = performance.now() - start;
			});
		});
	});
</script>

<svelte:head><title>Chart fixtures — dev</title></svelte:head>

<main>
	<h1>Chart layer fixtures</h1>
	<p class="lede">
		Not part of the app's nav — a living test page for every chart type in <code>$lib/charts</code>,
		including a large-N dataset for the perf-verification Phase 5 task 4 asked for. Fixture
		generation: realistic time-series {realisticTSMs.toFixed(1)}ms for {realisticTS.rows.length} rows;
		large time-series {largeTSGenMs.toFixed(1)}ms for {largeTS.rows.length} rows.
	</p>

	<div class="grid">
		<Card title="Time-series — multi-series overlay, legend toggle, zoom/pan (realistic: 3 services × ~480pts)">
			<TimeSeriesChart result={realisticTS} config={{ series_column: 'service' }} />
		</Card>

		<Card title="Bar — stacked by severity">
			<BarChart result={stacked} config={{ series_column: 'severity', stacked: 'true' }} />
		</Card>

		<Card title="Single-stat — sparkline + trend">
			<SingleStat result={statSeries} config={{ unit: 'ms', higher_is_worse: 'true' }} />
		</Card>

		<Card title="Heatmap — hour × day log volume">
			<Heatmap result={heat} config={{ x_column: 'hour', y_column: 'day', value_column: 'count' }} />
		</Card>

		<Card title="Top-N — ranked horizontal bar">
			<TopN result={ranked} />
		</Card>
	</div>

	<h2>Perf stress case</h2>
	<p class="lede">
		{largeTS.rows.length.toLocaleString()} rows, {SERVICES.length} series, canvas renderer.
		{#if largeRenderMs !== null}
			First two painted frames after mount: <strong>{largeRenderMs.toFixed(0)}ms</strong>.
		{/if}
		Try zooming (drag on the chart or the bottom slider) and toggling a legend entry — both should
		stay responsive at this volume.
	</p>
	<Card>
		<TimeSeriesChart result={largeTS} config={{ series_column: 'service' }} height="360px" />
	</Card>
</main>

<style>
	main {
		max-width: 75rem;
	}
	h1 {
		font-size: var(--text-xl);
		margin-bottom: var(--space-2);
	}
	h2 {
		font-size: var(--text-lg);
		margin: var(--space-6) 0 var(--space-2);
	}
	.lede {
		color: var(--color-text-muted);
		max-width: 70ch;
	}
	.grid {
		display: grid;
		grid-template-columns: repeat(auto-fit, minmax(22rem, 1fr));
		gap: var(--space-4);
		margin-top: var(--space-4);
	}
</style>
