<script lang="ts">
	// Shown on the landing page of a public-demo deployment only (see
	// $lib/api.ts's isPublicDemo). Says three things a visitor would
	// otherwise have to guess at, and would eventually be misled by: that
	// none of this data is real, that anything they do here is gone by
	// morning, and which features are deliberately limited on the demo
	// rather than broken.
	//
	// Colour comes from --color-accent, which the design system reserves
	// for interactive elements and explicitly bars from severity use.
	// That's the right side of the line: this is a UI notice about the
	// deployment, not a claim about any log record's severity -- the same
	// reasoning ui/Badge.svelte's `accent` tone already follows.
	import { Badge } from '$lib/components/ui';

	const points: { label: string; body: string }[] = [
		{
			label: 'Synthetic data',
			body: 'Every log line comes from a simulated fleet — 14 hosts running nginx, an API tier, workers, Postgres, Redis, mail, Linux journals and Windows event logs. No real system is being monitored. There are incidents seeded in the last few days worth finding, and new events keep arriving while you browse.'
		},
		{
			label: 'Resets nightly',
			body: 'The whole database is wiped and reseeded at 04:00 UTC so timestamps stay recent. Anything you change here is gone after that — and the fleet is regenerated, not restored.'
		},
		{
			label: 'Some limits',
			body: "You're signed in to a read-only account, so creating and editing are disabled. Alert rules evaluate for real, but notify a placeholder webhook that goes nowhere. Dashboards have no time-series charts yet: the query language can't bucket by time."
		}
	];
</script>

<aside>
	<header>
		<Badge tone="accent">Demo</Badge>
		<h2>You're looking at a simulation</h2>
	</header>
	<dl>
		{#each points as p (p.label)}
			<div class="point">
				<dt>{p.label}</dt>
				<dd>{p.body}</dd>
			</div>
		{/each}
	</dl>
</aside>

<style>
	aside {
		width: 100%;
		margin-top: var(--space-6);
		padding: var(--space-5) var(--space-5) var(--space-5) var(--space-4);
		text-align: left;
		border: 1px solid color-mix(in srgb, var(--color-accent) 30%, var(--color-border));
		/* The left rule is what makes it read as a notice at a glance,
		   before any of the text is parsed. */
		border-left: 3px solid var(--color-accent);
		border-radius: var(--radius-md);
		background:
			linear-gradient(
				to bottom right,
				color-mix(in srgb, var(--color-accent) 9%, transparent),
				color-mix(in srgb, var(--color-accent) 3%, transparent)
			),
			var(--color-surface);
	}
	header {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		margin-bottom: var(--space-4);
		padding-bottom: var(--space-3);
		border-bottom: 1px solid color-mix(in srgb, var(--color-accent) 20%, transparent);
	}
	h2 {
		margin: 0;
		font-size: var(--text-lg);
		font-weight: var(--font-weight-medium);
		color: var(--color-text);
	}
	dl {
		margin: 0;
		display: grid;
		grid-template-columns: repeat(3, 1fr);
		gap: var(--space-4);
	}
	.point {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}
	dt {
		font-size: var(--text-xs);
		font-weight: var(--font-weight-bold);
		letter-spacing: 0.06em;
		text-transform: uppercase;
		color: var(--color-accent);
	}
	dd {
		margin: 0;
		font-size: var(--text-sm);
		line-height: var(--line-height-normal);
		color: var(--color-text-muted);
	}
	/* Three columns need the room; below that they stack, which also
	   suits the narrow single-column layout the shortcuts grid drops to
	   at the same breakpoint. */
	@media (max-width: 52rem) {
		dl {
			grid-template-columns: 1fr;
			gap: var(--space-3);
		}
	}
</style>
