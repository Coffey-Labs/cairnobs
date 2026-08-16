<script lang="ts">
	import type { Snippet } from 'svelte';

	type Variant = 'primary' | 'secondary' | 'ghost' | 'danger';
	type Size = 'sm' | 'md';

	let {
		variant = 'secondary',
		size = 'md',
		type = 'button',
		disabled = false,
		href,
		onclick,
		children,
		...rest
	}: {
		variant?: Variant;
		size?: Size;
		type?: 'button' | 'submit';
		disabled?: boolean;
		href?: string;
		onclick?: (e: MouseEvent) => void;
		children: Snippet;
		[key: string]: unknown;
	} = $props();
</script>

{#if href}
	<a {href} class="btn {variant} {size}" aria-disabled={disabled} {...rest}>
		{@render children()}
	</a>
{:else}
	<button {type} class="btn {variant} {size}" {disabled} {onclick} {...rest}>
		{@render children()}
	</button>
{/if}

<style>
	.btn {
		display: inline-flex;
		align-items: center;
		justify-content: center;
		gap: var(--space-2);
		font-family: var(--font-ui);
		font-weight: var(--font-weight-medium);
		border-radius: var(--radius-sm);
		border: 1px solid transparent;
		cursor: pointer;
		text-decoration: none;
		white-space: nowrap;
		transition:
			background-color 0.1s ease,
			border-color 0.1s ease;
	}
	.btn:disabled,
	.btn[aria-disabled='true'] {
		opacity: 0.5;
		cursor: not-allowed;
	}

	.md {
		height: var(--control-height);
		padding: 0 var(--space-4);
		font-size: var(--text-base);
	}
	.sm {
		height: 1.75rem;
		padding: 0 var(--space-3);
		font-size: var(--text-sm);
	}

	.primary {
		background: var(--color-accent);
		color: var(--color-on-accent);
	}
	.primary:hover:not(:disabled) {
		background: var(--color-accent-strong);
	}

	.secondary {
		background: var(--color-surface-raised);
		color: var(--color-text);
		border-color: var(--color-border);
	}
	.secondary:hover:not(:disabled) {
		border-color: var(--color-border-strong);
	}

	.ghost {
		background: transparent;
		color: var(--color-text-muted);
	}
	.ghost:hover:not(:disabled) {
		background: var(--color-surface-raised);
		color: var(--color-text);
	}

	.danger {
		background: transparent;
		color: var(--color-danger);
		border-color: var(--color-danger);
	}
	.danger:hover:not(:disabled) {
		background: color-mix(in srgb, var(--color-danger) 12%, transparent);
	}
</style>
