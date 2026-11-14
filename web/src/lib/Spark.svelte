<script lang="ts">
	// A small trend line. Readings are drawn in --data (sig blue).
	let { data, max = 0, label }: { data: number[]; max?: number; label: string } = $props();
	const pts = $derived.by(() => {
		if (data.length < 2) return '';
		const top = max || Math.max(...data) * 1.1 || 1;
		return data.map((v, i) => `${((i / (data.length - 1)) * 100).toFixed(2)},${(30 - (v / top) * 28).toFixed(2)}`).join(' ');
	});
</script>

<svg class="spark" viewBox="0 0 100 32" preserveAspectRatio="none" role="img" aria-label={label}>
	{#if pts}
		<polygon points="0,32 {pts} 100,32" fill="var(--data)" opacity=".12" />
		<polyline points={pts} fill="none" stroke="var(--data)" stroke-width="2" vector-effect="non-scaling-stroke" stroke-linejoin="round" />
	{:else}
		<line x1="0" x2="100" y1="31" y2="31" stroke="var(--border)" vector-effect="non-scaling-stroke" />
	{/if}
</svg>

<style>
	.spark { display: block; width: 100%; height: 36px; }
</style>
