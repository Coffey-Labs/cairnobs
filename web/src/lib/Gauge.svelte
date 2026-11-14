<script lang="ts">
	// The console's dial: a 240° sweep with ticks, the reading in the middle.
	// Healthy readings are --data blue; tone switches it to a status colour.
	let { value, reading, unit = '', label, detail = '', tone = 'data', bands = [] }: {
		value: number; reading: string | number; unit?: string; label: string; detail?: string;
		tone?: 'data' | 'warning' | 'critical'; bands?: { from: number; to: number; tone: 'warning' | 'critical' }[];
	} = $props();

	const SWEEP = 240, START = 90 + (360 - SWEEP) / 2, R = 78, C = 100;
	const pt = (a: number, r: number) => { const x = (a * Math.PI) / 180; return [C + r * Math.cos(x), C + r * Math.sin(x)]; };
	const arc = (f: number, t: number, r: number) => {
		const a0 = START + SWEEP * f, a1 = START + SWEEP * t;
		const [x0, y0] = pt(a0, r), [x1, y1] = pt(a1, r);
		return `M ${x0} ${y0} A ${r} ${r} 0 ${a1 - a0 > 180 ? 1 : 0} 1 ${x1} ${y1}`;
	};
	const id = 'g' + Math.random().toString(36).slice(2);
	const v = $derived(Math.min(1, Math.max(0, value)));
	const len = (SWEEP / 360) * 2 * Math.PI * R;
	const needle = $derived(pt(START + SWEEP * v, R));
	const color = $derived(`var(--${tone})`);
	const ticks = Array.from({ length: 11 }, (_, i) => {
		const a = START + (SWEEP * i) / 10, major = i % 5 === 0;
		return { p0: pt(a, R + 10), p1: pt(a, R + (major ? 17 : 14)), major };
	});
</script>

<div class="gauge">
	<div class="dial">
		<svg viewBox="0 0 200 172" role="img" aria-label="{label}: {reading}{unit}">
			<defs><filter id={id} x="-50%" y="-50%" width="200%" height="200%"><feGaussianBlur stdDeviation="4" result="b" /><feMerge><feMergeNode in="b" /><feMergeNode in="SourceGraphic" /></feMerge></filter></defs>
			{#each ticks as t}<line x1={t.p0[0]} y1={t.p0[1]} x2={t.p1[0]} y2={t.p1[1]} stroke="var(--muted-fg)" stroke-opacity={t.major ? 0.7 : 0.35} stroke-width={t.major ? 1.6 : 1} stroke-linecap="round" />{/each}
			<path d={arc(0, 1, R)} fill="none" stroke="var(--muted)" stroke-width="12" stroke-linecap="round" />
			{#each bands as b}<path d={arc(b.from, b.to, R + 10)} fill="none" stroke="var(--{b.tone})" stroke-opacity=".55" stroke-width="2.5" />{/each}
			<path d={arc(0, 1, R)} fill="none" stroke={color} stroke-width="12" stroke-linecap="round" stroke-dasharray={len} stroke-dashoffset={(1 - v) * len} filter="url(#{id})" />
			<circle cx={needle[0]} cy={needle[1]} r="4.5" fill="var(--card)" stroke={color} stroke-width="2.5" />
		</svg>
		<div class="rd">{reading}{#if unit}<small>{unit}</small>{/if}</div>
	</div>
	<div class="lab">{label}</div>
	{#if detail}<div class="det">{detail}</div>{/if}
</div>

<style>
	.gauge { display: flex; flex-direction: column; align-items: center; }
	.dial { position: relative; width: 100%; max-width: 200px; }
	svg { width: 100%; overflow: visible; display: block; }
	.rd { position: absolute; inset: 34% 0 auto 0; display: flex; justify-content: center; align-items: baseline; gap: 3px; font: 600 27px var(--mono); letter-spacing: -.02em; }
	.rd small { font: 500 14px var(--mono); color: var(--muted-fg); }
	.lab { margin-top: -22px; font-size: 11px; font-weight: 600; letter-spacing: .14em; text-transform: uppercase; color: var(--muted-fg); text-align: center; }
	.det { margin-top: 4px; font-size: 12px; color: var(--muted-fg); text-align: center; }
</style>
