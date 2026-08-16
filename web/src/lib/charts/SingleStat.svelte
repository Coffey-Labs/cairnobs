<script lang="ts">
	// Big number + sparkline + trend, from one time-series-shaped result
	// (the same {columns, rows} a line panel gets -- the last value is
	// "now", the series is the sparkline, first-vs-last is the trend).
	// Trend color is neutral by default (whether "up" is good or bad
	// depends entirely on the metric -- request rate vs. error rate mean
	// opposite things) -- config.higher_is_worse opts a panel into
	// coloring an increase as the "error" severity tier instead of
	// leaving it neutral, for exactly the panels (error rate, queue
	// depth) where that framing is actually true.
	import { readChartTokens } from './theme';
	import { pivot } from './pivot';
	import type { QueryResult } from '$lib/api';

	let {
		result,
		config = {}
	}: {
		result: QueryResult;
		config?: { x_column?: string; value_column?: string; higher_is_worse?: string; unit?: string };
	} = $props();

	let stat = $derived.by(() => {
		const p = pivot(result.columns, result.rows, { xColumn: config.x_column, valueColumn: config.value_column });
		const series = p.series[0]?.data ?? [];
		const values = series.map((d) => d[1]);
		const current = values.length ? values[values.length - 1] : null;
		const first = values.length ? values[0] : null;
		const delta = current !== null && first !== null && first !== 0 ? ((current - first) / Math.abs(first)) * 100 : null;
		return { values, current, delta };
	});

	let t = $derived(readChartTokens());

	let trendColor = $derived.by(() => {
		if (stat.delta === null || stat.delta === 0) return t.textMuted;
		const worse = config.higher_is_worse === 'true' ? stat.delta > 0 : stat.delta < 0;
		return worse ? t.sevError : t.sevInfo;
	});

	function sparklinePath(values: number[], w: number, h: number): string {
		if (values.length < 2) return '';
		const min = Math.min(...values);
		const max = Math.max(...values);
		const range = max - min || 1;
		return values
			.map((v, i) => {
				const x = (i / (values.length - 1)) * w;
				const y = h - ((v - min) / range) * h;
				return `${i === 0 ? 'M' : 'L'}${x.toFixed(1)},${y.toFixed(1)}`;
			})
			.join(' ');
	}
</script>

<div class="single-stat">
	<div class="value">
		{stat.current !== null ? stat.current.toLocaleString() : '—'}
		{#if config.unit}<span class="unit">{config.unit}</span>{/if}
		{#if stat.delta !== null}
			<span class="trend" style:color={trendColor}>
				{stat.delta >= 0 ? '▲' : '▼'} {Math.abs(stat.delta).toFixed(1)}%
			</span>
		{/if}
	</div>
	{#if stat.values.length > 1}
		<svg class="sparkline" viewBox="0 0 120 32" preserveAspectRatio="none">
			<path d={sparklinePath(stat.values, 120, 28)} fill="none" stroke={t.accent} stroke-width="2" />
			<circle
				cx={120}
				cy={28 - ((stat.values[stat.values.length - 1] - Math.min(...stat.values)) / (Math.max(...stat.values) - Math.min(...stat.values) || 1)) * 28}
				r="2.5"
				fill={t.accent}
			/>
		</svg>
	{/if}
</div>

<style>
	.single-stat {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		padding: var(--space-2) 0;
	}
	.value {
		font-family: var(--font-mono);
		font-size: var(--text-2xl);
		font-weight: var(--font-weight-bold);
		color: var(--color-text);
		display: flex;
		align-items: baseline;
		gap: var(--space-2);
	}
	.unit {
		font-size: var(--text-md);
		color: var(--color-text-muted);
	}
	.trend {
		font-size: var(--text-sm);
		font-weight: var(--font-weight-medium);
		margin-left: auto;
	}
	.sparkline {
		width: 100%;
		height: 2rem;
		display: block;
	}
</style>
