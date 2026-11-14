<script lang="ts">
	let { label, value, warn = 80, crit = 90 }: { label: string; value: number; warn?: number; crit?: number } = $props();
</script>

<div class="meter" class:warn={value >= warn && value < crit} class:crit={value >= crit}>
	<div class="ml"><span>{label}</span><b>{Math.round(value)}%</b></div>
	<div class="bar" role="meter" aria-label={label} aria-valuenow={Math.round(value)} aria-valuemin={0} aria-valuemax={100}>
		<i style="width:{Math.min(100, Math.max(0, value))}%"></i>
	</div>
</div>
