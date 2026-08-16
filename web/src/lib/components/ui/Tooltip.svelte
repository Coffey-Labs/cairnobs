<script lang="ts">
	import type { Snippet } from 'svelte';

	let { text, children, id }: { text: string; children: Snippet; id: string } = $props();
</script>

<span class="wrap">
	<span aria-describedby={id} class="trigger">
		{@render children()}
	</span>
	<span role="tooltip" {id} class="tip">{text}</span>
</span>

<style>
	.wrap {
		position: relative;
		display: inline-flex;
	}
	.tip {
		position: absolute;
		bottom: calc(100% + var(--space-2));
		left: 50%;
		transform: translateX(-50%) translateY(2px);
		background: var(--color-surface-raised);
		border: 1px solid var(--color-border);
		color: var(--color-text);
		font-size: var(--text-xs);
		padding: var(--space-1) var(--space-2);
		border-radius: var(--radius-sm);
		white-space: nowrap;
		box-shadow: var(--shadow-sm);
		opacity: 0;
		visibility: hidden;
		pointer-events: none;
		transition:
			opacity 0.1s ease,
			transform 0.1s ease;
		z-index: 20;
	}
	.wrap:hover .tip,
	.wrap:focus-within .tip {
		opacity: 1;
		visibility: visible;
		transform: translateX(-50%) translateY(0);
	}
</style>
