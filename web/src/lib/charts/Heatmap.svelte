<script lang="ts">
	// Two-dimensional heatmap -- the shape a "log volume over time"
	// panel needs: two grouping columns (e.g. hour-of-day x day, or
	// service x severity) plus a count column, from a
	// `stats count by a, b` query. visualMap drives the color scale from
	// --color-surface (no data) up through --color-accent (most data) --
	// reuses the brand accent rather than introducing an unrelated third
	// color scale, since a heatmap's color IS its data encoding, not a
	// severity signal.
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
		config?: { x_column?: string; y_column?: string; value_column?: string };
		onDrillDown?: (point: { seriesName?: string; name: string; value: unknown }) => void;
		height?: string;
	} = $props();

	let option = $derived.by((): EChartsOption => {
		const t = readChartTokens();
		const { columns, rows } = result;
		if (columns.length < 2 || rows.length === 0) {
			return { ...baseOption(t), xAxis: { type: 'category', data: [] }, yAxis: { type: 'category', data: [] }, series: [] };
		}
		const xIdx = config.x_column ? columns.indexOf(config.x_column) : 0;
		const yIdx = config.y_column ? columns.indexOf(config.y_column) : 1;
		const valueIdx = config.value_column
			? columns.indexOf(config.value_column)
			: (columns.findIndex((_, i) => i !== xIdx && i !== yIdx) ?? 2);

		const xCats: string[] = [];
		const yCats: string[] = [];
		const data: [number, number, number][] = [];
		let max = 0;
		for (const row of rows) {
			const xVal = String(row[xIdx] ?? '');
			const yVal = String(row[yIdx] ?? '');
			if (!xCats.includes(xVal)) xCats.push(xVal);
			if (!yCats.includes(yVal)) yCats.push(yVal);
			const v = typeof row[valueIdx] === 'number' ? (row[valueIdx] as number) : Number(row[valueIdx]) || 0;
			max = Math.max(max, v);
			data.push([xCats.indexOf(xVal), yCats.indexOf(yVal), v]);
		}

		return {
			...baseOption(t),
			grid: { ...baseOption(t).grid, right: 16 },
			xAxis: { ...baseOption(t).xAxis, type: 'category', data: xCats, splitArea: { show: true } },
			yAxis: { ...baseOption(t).yAxis, type: 'category', data: yCats, splitArea: { show: true } },
			visualMap: {
				min: 0,
				max: max || 1,
				show: false,
				inRange: { color: [t.surfaceRaised, t.accent] }
			},
			series: [
				{
					type: 'heatmap',
					data,
					itemStyle: { borderColor: t.surface, borderWidth: 2 },
					emphasis: { itemStyle: { borderColor: t.text, borderWidth: 1 } }
				}
			]
		};
	});
</script>

<EChart {option} {height} onPointClick={(pt) => onDrillDown?.(pt)} />
