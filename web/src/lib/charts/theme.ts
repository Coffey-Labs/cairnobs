// ECharts renders to canvas, not the DOM -- it needs concrete color
// values, not CSS var() references. This reads the real resolved value
// of each token this module cares about directly off <html> so charts
// always match whatever theme/density is currently active instead of
// carrying a second, hand-maintained copy of the palette that can drift
// from tokens.css.

export type ChartTokens = {
	text: string;
	textMuted: string;
	border: string;
	surface: string;
	surfaceRaised: string;
	accent: string;
	sevQuiet: string;
	sevInfo: string;
	sevWarn: string;
	sevError: string;
	sevCritical: string;
	fontUI: string;
	fontMono: string;
};

function cssVar(name: string): string {
	return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
}

// Dark-mode literal fallback for prerendering/SSR, where `document`
// doesn't exist -- adapter-static prerenders every route (including
// dev/charts) at build time, and each chart's `option` is a $derived
// that runs during that pass same as it does in the browser. The real
// values always take over immediately once the client mounts; this
// only has to look reasonable for the static HTML shell, not be
// theme-accurate (prerendering can't know the visitor's theme choice
// anyway).
const SSR_FALLBACK: ChartTokens = {
	text: '#f0f0f1',
	textMuted: '#85888d',
	border: '#2a2c2f',
	surface: '#17181a',
	surfaceRaised: '#1e2023',
	accent: '#3fb6ff',
	sevQuiet: '#85888d',
	sevInfo: '#4c8dff',
	sevWarn: '#f5c242',
	sevError: '#ff6a39',
	sevCritical: '#ff2d78',
	fontUI: 'Overpass, sans-serif',
	fontMono: 'Overpass Mono, monospace'
};

export function readChartTokens(): ChartTokens {
	if (typeof document === 'undefined') return SSR_FALLBACK;
	return {
		text: cssVar('--color-text'),
		textMuted: cssVar('--color-text-muted'),
		border: cssVar('--color-border'),
		surface: cssVar('--color-surface'),
		surfaceRaised: cssVar('--color-surface-raised'),
		accent: cssVar('--color-accent'),
		sevQuiet: cssVar('--color-sev-quiet'),
		sevInfo: cssVar('--color-sev-info'),
		sevWarn: cssVar('--color-sev-warn'),
		sevError: cssVar('--color-sev-error'),
		sevCritical: cssVar('--color-sev-critical'),
		fontUI: cssVar('--font-ui'),
		fontMono: cssVar('--font-mono')
	};
}

// A fixed categorical palette for multi-series charts (hosts, services,
// etc. -- data that isn't severity-shaped, so severity's blue/amber/
// orange/magenta ramp doesn't apply). Chosen to stay distinguishable
// from the four severity colors above (no orange/magenta/amber-gold
// here) so a legend never makes a non-severity series look like it's
// signaling a severity. Colorblind-conscious: alternates hue and
// lightness, not just hue.
export const SERIES_PALETTE = [
	'#3fb6ff', // accent blue
	'#6fd6b0', // teal-green
	'#b48cff', // violet
	'#5c8dff', // periwinkle
	'#4dd0e1', // cyan
	'#8bc34a', // olive-green
	'#7986cb', // indigo
	'#4db6ac' // seafoam
];

// Shared base option every chart type extends -- background transparent
// (the card behind it supplies --color-surface), grid inset for axis
// labels, tooltip/legend/axis text all pulled from tokens so nothing is
// hardcoded per chart type.
export function baseOption(t: ChartTokens) {
	return {
		backgroundColor: 'transparent',
		textStyle: { fontFamily: t.fontUI, color: t.text },
		grid: { left: 48, right: 16, top: 28, bottom: 28, containLabel: true },
		tooltip: {
			backgroundColor: t.surfaceRaised,
			borderColor: t.border,
			borderWidth: 1,
			textStyle: { color: t.text, fontFamily: t.fontMono, fontSize: 12 },
			extraCssText: 'box-shadow: var(--shadow-md); border-radius: 6px;'
		},
		legend: {
			textStyle: { color: t.textMuted, fontFamily: t.fontUI, fontSize: 12 },
			inactiveColor: t.border,
			top: 0
		},
		xAxis: {
			axisLine: { lineStyle: { color: t.border } },
			axisLabel: { color: t.textMuted, fontFamily: t.fontMono, fontSize: 11 },
			splitLine: { show: false }
		},
		yAxis: {
			axisLine: { show: false },
			axisLabel: { color: t.textMuted, fontFamily: t.fontMono, fontSize: 11 },
			splitLine: { lineStyle: { color: t.border, type: 'dashed' as const } }
		}
	};
}
