<script lang="ts">
	// Multi-series time-series line, with legend-toggle (native ECharts
	// legend behavior -- click a series name to hide/show it) and
	// zoom/pan (dataZoom's slider + mouse-wheel/drag "inside" zoom) that
	// reports the zoomed range back via onZoom, for the caller to feed
	// into the dashboard's global time-range picker.
	import EChart from './EChart.svelte';
	import { readChartTokens, baseOption, SERIES_PALETTE } from './theme';
	import { pivot } from './pivot';
	import { getTimezone } from '$lib/timezone.svelte';
	import { axisTimeLabel, formatTimestamp } from '$lib/time';
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
		const zone = getTimezone();
		// Span drives label granularity; measured across every series,
		// since one may cover a wider range than another.
		const xs = p.series.flatMap((s) => s.data.map((d) => Number(d[0]))).filter((n) => !Number.isNaN(n));
		const spanMs = xs.length > 1 ? Math.max(...xs) - Math.min(...xs) : 0;

		return {
			...baseOption(t),
			color: SERIES_PALETTE,
			legend: multi ? { ...baseOption(t).legend, show: true } : { show: false },
			// Built as two concrete axes rather than one object with a
			// conditional `type`: a time axis and a category axis are
			// different option types, and merging them into one shape is
			// what TypeScript (correctly) refuses.
			xAxis: p.isTime
				? {
						...baseOption(t).xAxis,
						type: 'time' as const,
						axisLabel: {
							...baseOption(t).xAxis.axisLabel,
							formatter: (value: number) => axisTimeLabel(value, zone, spanMs)
						}
					}
				: { ...baseOption(t).xAxis, type: 'category' as const, data: p.categories },
			tooltip: p.isTime
				? {
						...baseOption(t).tooltip,
						trigger: 'axis' as const,
						// Same reason as the axis labels: ECharts' default
						// tooltip header is the browser's zone, which would
						// disagree with everything else on the page.
						formatter: (params: unknown) => {
							const rows = Array.isArray(params) ? params : [params];
							const first = rows[0] as { value?: [number, number] };
							const at = first?.value?.[0];
							const head =
								at === undefined
									? ''
									: `${formatTimestamp(new Date(at).toISOString(), zone)} ${zone}`;
							const body = rows
								.map((r) => {
									const s = r as { marker?: string; seriesName?: string; value?: [number, number] };
									return `${s.marker ?? ''}${s.seriesName ?? ''} ${s.value?.[1] ?? ''}`;
								})
								.join('<br/>');
							return [head, body].filter(Boolean).join('<br/>');
						}
					}
				: baseOption(t).tooltip,
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
