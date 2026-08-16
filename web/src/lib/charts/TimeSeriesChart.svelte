<script lang="ts">
	// Multi-series time-series line, with legend-toggle (native ECharts
	// legend behavior -- click a series name to hide/show it) and
	// zoom/pan (dataZoom's slider + mouse-wheel/drag "inside" zoom) that
	// reports the zoomed range back via onZoom, for the caller to feed
	// into the dashboard's global time-range picker.
	import EChart from './EChart.svelte';
	import { readChartTokens, baseOption, SERIES_PALETTE } from './theme';
	import { pivot } from './pivot';
	import type { QueryResult } from '$lib/api';
	import type { EChartsOption } from './setup';

	let {
		result,
		config = {},
		onDrillDown,
		onZoom,
		height = '260px'
	}: {
		result: QueryResult;
		config?: { x_column?: string; value_column?: string; series_column?: string };
		onDrillDown?: (point: { seriesName?: string; name: string; value: unknown }) => void;
		onZoom?: (range: { startValue?: number; endValue?: number }) => void;
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
			dataZoom: p.isTime
				? [
						{ type: 'inside', xAxisIndex: 0 },
						{ type: 'slider', xAxisIndex: 0, height: 16, bottom: 2, borderColor: t.border, fillerColor: `${t.accent}22` }
					]
				: undefined,
			series: p.series.map((s) => ({
				type: 'line',
				name: s.name,
				data: s.data,
				showSymbol: s.data.length < 80,
				symbolSize: 5,
				smooth: false,
				lineStyle: { width: 2 },
				areaStyle: multi ? undefined : { opacity: 0.12 }
			}))
		};
	});
</script>

<EChart
	{option}
	{height}
	onPointClick={(pt) => onDrillDown?.(pt)}
	onZoom={(range) => onZoom?.(range)}
/>
