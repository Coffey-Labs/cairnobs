<script lang="ts">
	// Dispatches a query result to the right rendering for a panel's
	// viz_type. table/top_n reuse ResultsTable.svelte (top_n is "table,
	// but the query already did sort/head" -- same execution path per
	// /docs/phase-3-dashboard-design.md). line/bar use uPlot.
	import uPlot from 'uplot';
	import 'uplot/dist/uPlot.min.css';
	import ResultsTable from '$lib/ResultsTable.svelte';
	import type { QueryResult, VizType } from '$lib/api';

	let {
		result,
		vizType,
		vizConfig = {}
	}: { result: QueryResult; vizType: VizType; vizConfig?: Record<string, string> } = $props();

	let chartEl: HTMLDivElement | undefined = $state();
	let chart: uPlot | undefined;

	// ISO-8601-ish prefix ("2026-08-13T20:20:24...") -- the shape Sentry's
	// own timestamp column actually comes back as. Deliberately narrow:
	// found by actually rendering a `stats count by host` bar chart that
	// JS's built-in Date.parse() is far too lenient to use as a "does
	// this look like a timestamp" check -- Date.parse("host-06") returns
	// a real (bogus) timestamp rather than NaN, which silently misrouted
	// a categorical `host` column onto a numeric time axis and rendered
	// unreadable giant tick labels instead of host names.
	const isoTimestampPrefix = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}/;

	// Prefers a real numeric/time x-axis when the column parses cleanly
	// (e.g. a timestamp column); falls back to row index with the raw
	// value as a tick label otherwise (e.g. a `stats ... by host` grouping
	// column, which is categorical text).
	function resolveXAxis(columns: string[], rows: unknown[][], columnName: string | undefined) {
		const idx = columnName ? Math.max(columns.indexOf(columnName), 0) : 0;
		const labels = rows.map((r) => String(r[idx] ?? ''));
		const asSeconds = rows.map((r) => {
			const v = r[idx];
			if (typeof v === 'number') return v;
			if (typeof v !== 'string' || !isoTimestampPrefix.test(v)) return NaN;
			const parsed = Date.parse(v);
			return Number.isNaN(parsed) ? NaN : parsed / 1000;
		});
		const allNumeric = asSeconds.every((n) => !Number.isNaN(n));
		return allNumeric
			? { values: asSeconds, labels, categorical: false }
			: { values: rows.map((_, i) => i), labels, categorical: true };
	}

	function resolveValueColumn(columns: string[], columnName: string | undefined): number {
		if (columnName) {
			const i = columns.indexOf(columnName);
			if (i >= 0) return i;
		}
		// default: second column if there is one (first is usually the
		// grouping/x column), otherwise the only column there is.
		return columns.length > 1 ? 1 : 0;
	}

	function renderChart() {
		if (!chartEl) return;
		chart?.destroy();
		chart = undefined;
		if (vizType !== 'line' && vizType !== 'bar') return;

		const { columns, rows } = result;
		if (columns.length === 0 || rows.length === 0) return;

		const x = resolveXAxis(columns, rows, vizConfig.x_column);
		const valueIdx = resolveValueColumn(columns, vizConfig.value_column);
		const values = rows.map((r) => {
			const v = r[valueIdx];
			return typeof v === 'number' ? v : Number(v) || 0;
		});

		chart = new uPlot(
			{
				width: chartEl.clientWidth || 400,
				height: 220,
				legend: { show: false },
				series: [
					{},
					{
						label: columns[valueIdx],
						stroke: '#06c',
						fill: vizType === 'bar' ? '#06c33' : undefined,
						width: vizType === 'bar' ? 0 : 2,
						paths: vizType === 'bar' ? uPlot.paths.bars!({ size: [0.6] }) : undefined
					}
				],
				axes: [
					{
						values: (_u, ticks) =>
							ticks.map((t) => (x.categorical ? (x.labels[t] ?? '') : String(t)))
					},
					{}
				]
			},
			[x.values, values],
			chartEl
		);
	}

	$effect(() => {
		// Re-render whenever the result or viz settings change.
		result;
		vizType;
		vizConfig;
		renderChart();
		return () => chart?.destroy();
	});

	// A dashboard panel's real width isn't known at first render --
	// gridstack.js sizes the parent .grid-stack-item via its own layout
	// pass, which can land after this component's own effect runs. Found
	// by actually adding a bar-chart panel and inspecting the rendered
	// canvas: it came out 74px wide (chartEl.clientWidth measured before
	// gridstack finished sizing the container), not the container's real
	// ~540px. A ResizeObserver re-renders whenever chartEl's actual size
	// changes, which fixes both that initial race and, as a side benefit,
	// keeps the chart correctly sized when a panel is drag-resized later.
	$effect(() => {
		if (!chartEl) return;
		const observer = new ResizeObserver(() => renderChart());
		observer.observe(chartEl);
		return () => observer.disconnect();
	});
</script>

{#if vizType === 'table' || vizType === 'top_n'}
	<ResultsTable columns={result.columns} rows={result.rows} hasRun={true} />
{:else if vizType === 'single_stat'}
	<div class="single-stat">{result.rows[0]?.[0] ?? '—'}</div>
{:else}
	<div bind:this={chartEl} class="chart"></div>
{/if}

<style>
	.single-stat {
		font-size: 2.5rem;
		font-weight: 600;
		padding: 1rem 0;
	}
	.chart {
		width: 100%;
	}
</style>
