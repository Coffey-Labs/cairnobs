<script lang="ts">
	// The last lines of a container's log, folded away until asked for.
	let { lines, open = false }: { lines: string[]; open?: boolean } = $props();
</script>

<details class="logs" {open}>
	<summary>Last {lines.length} log {lines.length === 1 ? 'line' : 'lines'}</summary>
	<pre>{lines.join('\n')}</pre>
</details>

<style>
	.logs { margin-top: 8px; }
	summary { cursor: pointer; font-size: 13px; color: var(--muted-fg); }
	summary:hover { color: var(--foreground); }
	pre {
		margin: 6px 0 0; padding: 10px 12px; max-height: 280px; overflow: auto;
		background: var(--background); border: 1px solid var(--border); border-radius: var(--radius);
		font: 12px/1.5 var(--mono); white-space: pre-wrap; overflow-wrap: anywhere;
	}
</style>
