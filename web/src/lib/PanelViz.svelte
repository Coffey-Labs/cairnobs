<script lang="ts">
	// Dispatches a query result to the right rendering for a panel's
	// viz_type. "table" still reuses ResultsTable.svelte; every chart
	// type is Phase 5's ECharts layer ($lib/charts), replacing Phase 3's
	// direct uPlot usage. top_n's *execution* is unchanged (see
	// api/dashboards/types.go's VizType doc comment -- the query itself
	// already did `stats ... | sort | head`), but now actually renders as
	// the ranked horizontal bar chart the name always implied, not a
	// plain table.
	import { goto } from '$app/navigation';
	import ResultsTable from '$lib/ResultsTable.svelte';
	import {
		TimeSeriesChart,
		BarChart,
		TopN,
		Heatmap,
		SingleStat,
		pivot,
		buildDrillDownQuery,
		drillDownUrl
	} from '$lib/charts';
	import type { QueryResult, VizType } from '$lib/api';

	let {
		result,
		vizType,
		vizConfig = {},
		query,
		onZoom
	}: {
		result: QueryResult;
		vizType: VizType;
		vizConfig?: Record<string, string>;
		// Needed to build a drill-down query (see $lib/charts/drilldown.ts);
		// undefined call sites just lose drill-down, not error.
		query?: string;
		// Bubbles a time-series chart's zoom range up to the dashboard so
		// it can become the new global time range -- see
		// dashboards/[id]/+page.svelte's onZoom handler.
		onZoom?: (range: { earliest: string; latest: string }) => void;
	} = $props();

	let isTimeSeries = $derived.by(() => {
		if (vizType !== 'line' && vizType !== 'bar') return false;
		return pivot(result.columns, result.rows, { xColumn: vizConfig.x_column }).isTime;
	});

	function handleDrillDown(point: { seriesName?: string; name: string; value: unknown }, isTime: boolean) {
		if (!query) return;
		// value is [x, y] for line/bar (ECharts passes the raw data tuple
		// back), or just the category label for TopN/Heatmap.
		const xValue = Array.isArray(point.value) ? point.value[0] : point.name;
		const target = buildDrillDownQuery(query, {
			seriesColumn: vizConfig.series_column,
			seriesName: point.seriesName,
			xValue,
			isTime
		});
		goto(drillDownUrl(target));
	}

	function handleZoom(range: { startValue?: number; endValue?: number }) {
		if (!onZoom || range.startValue == null || range.endValue == null) return;
		onZoom({
			earliest: new Date(range.startValue).toISOString(),
			latest: new Date(range.endValue).toISOString()
		});
	}
</script>

{#if vizType === 'table'}
	<ResultsTable columns={result.columns} rows={result.rows} hasRun={true} />
{:else if vizType === 'single_stat'}
	<SingleStat {result} config={vizConfig} />
{:else if vizType === 'heatmap'}
	<Heatmap {result} config={vizConfig} onDrillDown={(pt) => handleDrillDown(pt, false)} />
{:else if vizType === 'top_n'}
	<TopN {result} config={vizConfig} onDrillDown={(pt) => handleDrillDown(pt, false)} />
{:else if vizType === 'line'}
	<TimeSeriesChart
		{result}
		config={vizConfig}
		onDrillDown={(pt) => handleDrillDown(pt, isTimeSeries)}
		onZoom={handleZoom}
	/>
{:else if vizType === 'bar'}
	<BarChart {result} config={vizConfig} onDrillDown={(pt) => handleDrillDown(pt, isTimeSeries)} />
{/if}
