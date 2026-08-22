// Modular ECharts registration -- pulling in `echarts` (the full bundle)
// would ship every chart type/component ECharts has ever shipped,
// against the whole reason it was picked over hand-rolled D3 for the
// bundle-size tradeoff (see the Phase 5 charting-library review). This
// registers only what Cairn OBS's five chart types actually use: line/bar
// (time-series, stacked bar, top-N, the single-stat sparkline) and
// heatmap, plus tooltip/legend/grid/dataZoom/visualMap and the canvas
// renderer. Imported once, here, not per-component -- echarts.use() is
// idempotent but there's no reason to repeat the list five times.
import * as echarts from 'echarts/core';
import { LineChart, BarChart, HeatmapChart } from 'echarts/charts';
import {
	TooltipComponent,
	GridComponent,
	LegendComponent,
	DataZoomComponent,
	VisualMapComponent,
	MarkLineComponent
} from 'echarts/components';
import { CanvasRenderer } from 'echarts/renderers';

echarts.use([
	LineChart,
	BarChart,
	HeatmapChart,
	TooltipComponent,
	GridComponent,
	LegendComponent,
	DataZoomComponent,
	VisualMapComponent,
	MarkLineComponent,
	CanvasRenderer
]);

export { echarts };
export type EChartsOption = echarts.ComposeOption<
	| import('echarts/charts').LineSeriesOption
	| import('echarts/charts').BarSeriesOption
	| import('echarts/charts').HeatmapSeriesOption
	| import('echarts/components').TooltipComponentOption
	| import('echarts/components').GridComponentOption
	| import('echarts/components').LegendComponentOption
	| import('echarts/components').DataZoomComponentOption
	| import('echarts/components').VisualMapComponentOption
	| import('echarts/components').MarkLineComponentOption
>;
