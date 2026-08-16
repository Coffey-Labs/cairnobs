<script lang="ts">
	import type { Snippet } from 'svelte';

	let {
		value = $bindable(''),
		disabled = false,
		id,
		children,
		...rest
	}: {
		value?: string;
		disabled?: boolean;
		id?: string;
		children: Snippet;
		[key: string]: unknown;
	} = $props();
</script>

<div class="wrap">
	<select {id} {disabled} bind:value class="select" {...rest}>
		{@render children()}
	</select>
	<span class="chev" aria-hidden="true">⌄</span>
</div>

<style>
	.wrap {
		position: relative;
		display: inline-block;
	}
	.select {
		appearance: none;
		height: var(--control-height);
		padding: 0 var(--space-6) 0 var(--space-3);
		background: var(--color-surface);
		border: 1px solid var(--color-border);
		border-radius: var(--radius-sm);
		color: var(--color-text);
		font-family: var(--font-ui);
		font-size: var(--text-base);
		width: 100%;
		cursor: pointer;
	}
	.select:hover:not(:disabled) {
		border-color: var(--color-border-strong);
	}
	.select:disabled {
		opacity: 0.5;
		cursor: not-allowed;
	}
	.chev {
		position: absolute;
		right: var(--space-3);
		top: 50%;
		transform: translateY(-50%);
		color: var(--color-text-muted);
		pointer-events: none;
		font-size: var(--text-sm);
	}
</style>
