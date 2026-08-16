<script lang="ts">
	// Bar chart, category or time x-axis, optionally stacked
	// (config.stacked -- multiple series sharing one `stack` group,
	// ECharts' native stacking, no manual cumulative-sum math needed).
	import EChart from './EChart.svelte';
	import { readChartTokens, baseOption, SERIES_PALETTE } from './theme';
	import { pivot } from './pivot';
	import type { QueryResult } from '$lib/api';
	import type { EChartsOption } from './setup';

	let {
		result,
		config = {},
		onDrillDown,
		height = '260px'
	}: {
		result: QueryResult;
		config?: { x_column?: string; value_column?: string; series_column?: string; stacked?: string };
		onDrillDown?: (point: { seriesName?: string; name: string; value: unknown }) => void;
		height?: string;
	} = $props();

	let option = $derived.by((): EChartsOption => {
		const t = readChartTokens();
		const p = pivot(result.columns, result.rows, {
			xColumn: config.x_column,
			valueColumn: config.value_column,
			seriesColumn: config.series_column
		});
		const multi = p.series.length > 1;
		const stacked = config.stacked === 'true';

		return {
			...baseOption(t),
			color: SERIES_PALETTE,
			legend: multi ? { ...baseOption(t).legend, show: true } : { show: false },
			xAxis: {
				...baseOption(t).xAxis,
				type: p.isTime ? 'time' : 'category',
				data: p.isTime ? undefined : p.categories
			},
			yAxis: { ...baseOption(t).yAxis, type: 'value' },
			series: p.series.map((s) => ({
				type: 'bar',
				name: s.name,
				data: s.data,
				stack: stacked ? 'total' : undefined,
				barMaxWidth: 28
			}))
		};
	});
</script>

<EChart {option} {height} onPointClick={(pt) => onDrillDown?.(pt)} />
