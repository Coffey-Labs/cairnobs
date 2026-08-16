<script lang="ts">
	import type { Snippet } from 'svelte';

	let {
		title,
		actions,
		padded = true,
		children
	}: {
		title?: string;
		actions?: Snippet;
		padded?: boolean;
		children: Snippet;
	} = $props();
</script>

<div class="card">
	{#if title || actions}
		<div class="head">
			{#if title}<h2>{title}</h2>{/if}
			{#if actions}<div class="actions">{@render actions()}</div>{/if}
		</div>
	{/if}
	<div class="body" class:padded>
		{@render children()}
	</div>
</div>

<style>
	.card {
		background: var(--color-surface);
		border: 1px solid var(--color-border);
		border-radius: var(--radius-md);
		overflow: hidden;
	}
	.head {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-3);
		padding: var(--space-3) var(--panel-padding);
		border-bottom: 1px solid var(--color-border);
	}
	.head h2 {
		font-size: var(--text-base);
		font-weight: var(--font-weight-medium);
	}
	.body.padded {
		padding: var(--panel-padding);
	}
</style>
