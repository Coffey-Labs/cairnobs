<script lang="ts">
	// A firing/resolved delivery log IS a state-transition history --
	// each row already carries a timestamp and an event_type, which is
	// exactly a timeline's raw material. No new backend data was needed
	// for "a timeline view of an alert's state history rather than just
	// a flat delivery log" (Phase 5 task 7); this is the same
	// DeliveryLogEntry list the old flat table read, framed differently.
	import { getTimezone } from '$lib/timezone.svelte';
	import { formatTimestamp } from '$lib/time';
	import type { DeliveryLogEntry } from '$lib/api';

	let { deliveries }: { deliveries: DeliveryLogEntry[] } = $props();

	function tierFor(d: DeliveryLogEntry): 'critical' | 'quiet' {
		return d.event_type === 'firing' ? 'critical' : 'quiet';
	}

	function statusLabel(d: DeliveryLogEntry): string {
		if (d.status === 'sent') return 'delivered';
		if (d.status === 'failed') return `failed${d.response_status ? ` (HTTP ${d.response_status})` : ''}`;
		if (d.status === 'retrying') return `retrying (attempt ${d.attempt_count})`;
		return d.status;
	}
</script>

{#if deliveries.length === 0}
	<p class="muted">No deliveries yet.</p>
{:else}
	<ol class="timeline">
		{#each deliveries as d (d.id)}
			<li>
				<span class="rail">
					<span class="dot {tierFor(d)}"></span>
				</span>
				<div class="entry">
					<div class="entry-head">
						<span class="event {tierFor(d)}">{d.event_type}</span>
						<time title={d.created_at}>{formatTimestamp(d.created_at, getTimezone())}</time>
					</div>
					<div class="entry-body">
						<span class:danger={d.status === 'failed'}>{statusLabel(d)}</span>
						{#if d.last_error}<span class="error-text">— {d.last_error}</span>{/if}
					</div>
				</div>
			</li>
		{/each}
	</ol>
{/if}

<style>
	.muted {
		color: var(--color-text-muted);
	}
	.timeline {
		list-style: none;
		margin: 0;
		padding: 0;
	}
	.timeline li {
		display: grid;
		grid-template-columns: 1.25rem 1fr;
		gap: var(--space-3);
	}
	.rail {
		display: flex;
		flex-direction: column;
		align-items: center;
	}
	.rail::before {
		content: '';
		width: 1px;
		flex: 1;
		background: var(--color-border);
	}
	.timeline li:first-child .rail::before {
		visibility: hidden;
	}
	.dot {
		width: 10px;
		height: 10px;
		border-radius: 50%;
		flex: none;
		margin-top: 0.35rem;
		border: 2px solid var(--color-bg);
		box-shadow: 0 0 0 1px var(--color-border);
	}
	.dot.critical {
		background: var(--color-sev-critical);
	}
	.dot.quiet {
		background: var(--color-sev-quiet);
	}
	.entry {
		padding-bottom: var(--space-4);
	}
	.entry-head {
		display: flex;
		align-items: baseline;
		gap: var(--space-2);
	}
	.event {
		font-weight: var(--font-weight-medium);
		text-transform: capitalize;
		font-size: var(--text-sm);
	}
	.event.critical {
		color: var(--color-sev-critical);
	}
	.event.quiet {
		color: var(--color-sev-quiet);
	}
	time {
		font-family: var(--font-mono);
		font-size: var(--text-xs);
		color: var(--color-text-muted);
	}
	.entry-body {
		font-size: var(--text-sm);
		color: var(--color-text-muted);
		margin-top: var(--space-1);
	}
	.danger {
		color: var(--color-danger);
	}
	.error-text {
		color: var(--color-danger);
	}
</style>
