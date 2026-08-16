<script lang="ts">
	// Base wrapper every chart type in this directory builds on. Owns the
	// echarts instance lifecycle (init/resize/dispose) and the two shared
	// interaction patterns every chart type needs identically: emitting a
	// click (for drill-down) and a dataZoom range (for the global
	// time-range picker) as plain callback props, not custom DOM events --
	// keeps call sites (PanelViz, dashboards) working with normal
	// function props instead of addEventListener-style wiring.
	import { echarts, type EChartsOption } from './setup';
	import { getTheme } from '$lib/theme.svelte';
	import type { EChartsType } from 'echarts/core';

	let {
		option,
		height = '100%',
		onPointClick,
		onZoom
	}: {
		option: EChartsOption;
		height?: string;
		onPointClick?: (params: { seriesName?: string; name: string; value: unknown }) => void;
		onZoom?: (range: { startValue?: number; endValue?: number }) => void;
	} = $props();

	let el: HTMLDivElement | undefined = $state();
	let chart: EChartsType | undefined;

	function render() {
		if (!el) return;
		if (!chart) {
			chart = echarts.init(el, undefined, { renderer: 'canvas' });
			chart.on('click', (params) => {
				onPointClick?.({ seriesName: params.seriesName, name: String(params.name), value: params.value });
			});
			chart.on('datazoom', () => {
				if (!chart || !onZoom) return;
				// finished/batch events both land here; read the resolved
				// window back off the model rather than trusting whichever
				// shape this particular event fired with.
				const opt = chart.getOption() as { dataZoom?: { startValue?: number; endValue?: number }[] };
				const dz = opt.dataZoom?.[0];
				if (dz) onZoom({ startValue: dz.startValue, endValue: dz.endValue });
			});
		}
		chart.setOption(option, { notMerge: true });
	}

	$effect(() => {
		option;
		getTheme(); // re-render on theme change -- token colors baked into `option` upstream need a fresh read
		render();
	});

	$effect(() => {
		if (!el) return;
		const observer = new ResizeObserver(() => chart?.resize());
		observer.observe(el);
		return () => observer.disconnect();
	});

	$effect(() => {
		return () => {
			chart?.dispose();
			chart = undefined;
		};
	});
</script>

<div bind:this={el} class="echart" style:height></div>

<style>
	.echart {
		width: 100%;
	}
</style>
