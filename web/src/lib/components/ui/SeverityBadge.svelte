<script lang="ts">
	import { severityTier } from '$lib/severity';

	let { severity }: { severity: string } = $props();
	let tier = $derived(severityTier(severity));
</script>

<span class="chip {tier}">{severity}</span>

<style>
	.chip {
		display: inline-flex;
		align-items: center;
		font-family: var(--font-mono);
		font-size: var(--text-xs);
		font-weight: var(--font-weight-bold);
		letter-spacing: 0.02em;
		padding: 0.05rem var(--space-2);
		border-radius: var(--radius-sm);
		width: fit-content;
	}
	.quiet {
		color: var(--color-sev-quiet);
		background: var(--color-sev-quiet-bg);
	}
	.info {
		color: var(--color-sev-info);
		background: var(--color-sev-info-bg);
	}
	.warn {
		color: var(--color-sev-warn);
		background: var(--color-sev-warn-bg);
	}
	.error {
		color: var(--color-sev-error);
		background: var(--color-sev-error-bg);
	}
	.critical {
		color: var(--color-sev-critical);
		background: var(--color-sev-critical-bg);
	}
</style>
