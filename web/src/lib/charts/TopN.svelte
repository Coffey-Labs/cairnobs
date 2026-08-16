<script lang="ts">
	// Horizontal bar, ranked. Mirrors table/top_n's existing execution
	// model (see api/dashboards/types.go's VizType doc comment): the
	// query itself already did `stats ... | sort -count | head N`, so
	// this only reframes already-ranked rows, no client-side re-sorting.
	import EChart from './EChart.svelte';
	import { readChartTokens, baseOption } from './theme';
	import type { QueryResult } from '$lib/api';
	import type { EChartsOption } from './setup';

	let {
		result,
		config = {},
		onDrillDown,
		height = '260px'
	}: {
		result: QueryResult;
		config?: { label_column?: string; value_column?: string };
		onDrillDown?: (point: { seriesName?: string; name: string; value: unknown }) => void;
		height?: string;
	} = $props();

	let option = $derived.by((): EChartsOption => {
		const t = readChartTokens();
		const { columns, rows } = result;
		if (columns.length === 0 || rows.length === 0) {
			return { ...baseOption(t), xAxis: { type: 'value' }, yAxis: { type: 'category', data: [] }, series: [] };
		}
		const labelIdx = config.label_column ? columns.indexOf(config.label_column) : 0;
		const valueIdx = config.value_column
			? columns.indexOf(config.value_column)
			: (columns.findIndex((_, i) => i !== labelIdx && typeof rows[0][i] === 'number') ?? 1);

		// Reversed: ECharts' category axis draws bottom-to-top, but a
		// ranked "top N" list reads top-to-bottom -- reversing the arrays
		// (rather than yAxis.inverse, which also flips axis-line
		// placement) keeps rank #1 visually on top without touching
		// anything else about the axis.
		const labels = rows.map((r) => String(r[labelIdx] ?? '')).reverse();
		const values = rows.map((r) => (typeof r[valueIdx] === 'number' ? (r[valueIdx] as number) : Number(r[valueIdx]) || 0)).reverse();

		return {
			...baseOption(t),
			grid: { ...baseOption(t).grid, left: 8 },
			xAxis: { ...baseOption(t).yAxis, type: 'value' },
			yAxis: {
				...baseOption(t).xAxis,
				type: 'category',
				data: labels,
				axisLabel: { ...baseOption(t).xAxis.axisLabel, width: 120, overflow: 'truncate' }
			},
			series: [
				{
					type: 'bar',
					data: values,
					color: t.accent,
					barMaxWidth: 20,
					label: { show: true, position: 'right', color: t.textMuted, fontFamily: t.fontMono, fontSize: 11 }
				}
			]
		};
	});
</script>

<EChart {option} {height} onPointClick={(pt) => onDrillDown?.(pt)} />
