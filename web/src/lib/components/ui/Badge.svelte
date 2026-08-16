<script lang="ts">
	import type { Snippet } from 'svelte';

	// Generic status badge -- for severity specifically, use
	// SeverityBadge.svelte instead (it owns the OTel->tier mapping so
	// callers can't accidentally invent a sixth color).
	type Tone = 'neutral' | 'success' | 'danger' | 'accent';

	let { tone = 'neutral', children }: { tone?: Tone; children: Snippet } = $props();
</script>

<span class="badge {tone}">{@render children()}</span>

<style>
	.badge {
		display: inline-flex;
		align-items: center;
		font-family: var(--font-ui);
		font-size: var(--text-xs);
		font-weight: var(--font-weight-bold);
		letter-spacing: 0.02em;
		padding: 0.1rem var(--space-2);
		border-radius: var(--radius-sm);
		width: fit-content;
	}
	.neutral {
		color: var(--color-text-muted);
		background: color-mix(in srgb, var(--color-text-muted) 16%, transparent);
	}
	.success {
		color: var(--color-success);
		background: color-mix(in srgb, var(--color-success) 16%, transparent);
	}
	.danger {
		color: var(--color-danger);
		background: color-mix(in srgb, var(--color-danger) 16%, transparent);
	}
	.accent {
		color: var(--color-accent);
		background: color-mix(in srgb, var(--color-accent) 16%, transparent);
	}
</style>
