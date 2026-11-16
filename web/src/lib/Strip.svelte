<script lang="ts">
	/** A service's last 24 hours: one bar per half hour, coloured by the worst status in it. */
	let { cells, name = '' }: { cells: string[]; name?: string } = $props();
	const bad = $derived(cells.filter((c) => c === 'warning' || c === 'serious' || c === 'critical').length);
	const label = $derived(`${name ? name + ', ' : ''}last 24 hours: ${bad ? `trouble in ${bad} of ${cells.length} half hours` : 'no trouble'}`);
</script>

<span class="strip" role="img" aria-label={label}>{#each cells as c, i (i)}<i class={c || 'none'}></i>{/each}</span>

<style>
	.strip { display: flex; gap: 1px; height: 22px; min-width: 0; }
	.strip i { flex: 1 1 0; min-width: 1px; border-radius: 2px; background: var(--border); }
	.strip .good { background: var(--good); opacity: .45; }
	.strip .off, .strip .waiting { background: var(--muted-fg); opacity: .3; }
	.strip .warning { background: var(--warning); }
	.strip .serious { background: var(--serious); }
	.strip .critical { background: var(--critical); }
</style>
