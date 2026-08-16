<script lang="ts">
	// "Not just a text label — use the severity color system" (Phase 5
	// task 7). Reuses the same four-tier severity tokens log data uses
	// (see $lib/severity.ts) rather than inventing a second color
	// vocabulary for alert state: ok -> quiet, pending -> warn,
	// firing -> critical. A dot, not just colored text, so state reads
	// at a glance in a dense rule list, not just on close reading.
	import type { AlertRuleState } from '$lib/api';

	let { state }: { state: AlertRuleState } = $props();

	const tierFor: Record<AlertRuleState, 'quiet' | 'warn' | 'critical'> = {
		ok: 'quiet',
		pending: 'warn',
		firing: 'critical'
	};
</script>

<span class="pill {tierFor[state]}">
	<span class="dot" aria-hidden="true"></span>
	{state}
</span>

<style>
	.pill {
		display: inline-flex;
		align-items: center;
		gap: var(--space-1);
		font-family: var(--font-ui);
		font-size: var(--text-xs);
		font-weight: var(--font-weight-medium);
		padding: 0.15rem var(--space-2);
		border-radius: var(--radius-full);
		text-transform: capitalize;
	}
	.dot {
		width: 6px;
		height: 6px;
		border-radius: 50%;
	}
	.quiet {
		color: var(--color-sev-quiet);
		background: var(--color-sev-quiet-bg);
	}
	.quiet .dot {
		background: var(--color-sev-quiet);
	}
	.warn {
		color: var(--color-sev-warn);
		background: var(--color-sev-warn-bg);
	}
	.warn .dot {
		background: var(--color-sev-warn);
	}
	.critical {
		color: var(--color-sev-critical);
		background: var(--color-sev-critical-bg);
	}
	.critical .dot {
		background: var(--color-sev-critical);
	}
</style>
