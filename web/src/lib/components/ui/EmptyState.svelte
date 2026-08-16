<script lang="ts">
	import type { Snippet } from 'svelte';

	let {
		icon = '◇',
		title,
		description,
		action
	}: {
		icon?: string;
		title: string;
		description?: string;
		action?: Snippet;
	} = $props();
</script>

<div class="empty">
	<div class="icon" aria-hidden="true">{icon}</div>
	<h2>{title}</h2>
	{#if description}<p>{description}</p>{/if}
	{#if action}<div class="action">{@render action()}</div>{/if}
</div>

<style>
	.empty {
		display: flex;
		flex-direction: column;
		align-items: center;
		text-align: center;
		gap: var(--space-2);
		padding: var(--space-8) var(--space-4);
		color: var(--color-text-muted);
	}
	.icon {
		font-size: var(--text-2xl);
		color: var(--color-border-strong);
		margin-bottom: var(--space-2);
	}
	h2 {
		font-size: var(--text-md);
		font-weight: var(--font-weight-medium);
		color: var(--color-text);
	}
	p {
		max-width: 30rem;
		margin: 0;
	}
	.action {
		margin-top: var(--space-2);
	}
</style>
