<script lang="ts">
	import type { Snippet } from 'svelte';

	// A thin styling wrapper, not a data grid -- real <table> markup
	// underneath (native semantics matter for screen readers), density-
	// aware row height/padding from tokens.css's --row-* variables.
	// Column sort/resize (task 6) layers on top of this later; this just
	// establishes the shared chrome every table in the app should share.
	let { children }: { children: Snippet } = $props();
</script>

<div class="scroll">
	<table class="ui-table">
		{@render children()}
	</table>
</div>

<style>
	.scroll {
		overflow-x: auto;
	}
	table {
		width: 100%;
		border-collapse: collapse;
		font-family: var(--font-mono);
		font-size: var(--text-base);
	}
	:global(.ui-table thead th) {
		text-align: left;
		font-family: var(--font-ui);
		font-size: var(--text-xs);
		font-weight: var(--font-weight-medium);
		text-transform: uppercase;
		letter-spacing: 0.04em;
		color: var(--color-text-muted);
		padding: var(--space-2) var(--row-padding-x);
		border-bottom: 1px solid var(--color-border);
		white-space: nowrap;
	}
	:global(.ui-table tbody td) {
		padding: var(--row-padding-y) var(--row-padding-x);
		border-bottom: 1px solid var(--color-border);
		color: var(--color-text);
		vertical-align: middle;
	}
	:global(.ui-table tbody tr:last-child td) {
		border-bottom: none;
	}
	:global(.ui-table tbody tr:hover td) {
		background: var(--color-surface-raised);
	}
</style>
