<script lang="ts">
	import { page } from '$app/state';
	import { getRule, deleteRule, listDeliveries, type AlertRule, type DeliveryLogEntry } from '$lib/api';
	import { Button } from '$lib/components/ui';
	import AlertStatePill from '$lib/components/AlertStatePill.svelte';
	import DeliveryTimeline from '$lib/components/DeliveryTimeline.svelte';

	const ruleId = page.params.id!;

	let rule = $state<AlertRule | null>(null);
	let deliveries = $state<DeliveryLogEntry[]>([]);
	let loading = $state(true);
	let error = $state('');

	async function load() {
		loading = true;
		error = '';
		try {
			rule = await getRule(ruleId);
			deliveries = await listDeliveries(ruleId);
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		} finally {
			loading = false;
		}
	}
	load();

	async function remove() {
		await deleteRule(ruleId);
		window.location.href = '/alerts';
	}

	function conditionSummary(r: AlertRule): string {
		if (r.condition_type === 'absence') return 'query returns zero rows in its own time window';
		const symbols: Record<string, string> = { gt: '>', gte: '>=', lt: '<', lte: '<=', eq: '==', ne: '!=' };
		return `first row's value ${symbols[r.comparator ?? ''] ?? r.comparator} ${r.threshold_value}`;
	}
</script>

<main>
	{#if loading}
		<p>Loading…</p>
	{:else if !rule}
		<p class="error">Error: {error}</p>
	{:else}
		<div class="header">
			<h1>{rule.name}</h1>
			<Button variant="danger" onclick={remove}>Delete rule</Button>
		</div>
		{#if rule.description}<p class="desc">{rule.description}</p>{/if}
		{#if error}<p class="error">Error: {error}</p>{/if}

		<section class="summary">
			<div>
				<span class="label">State</span>
				<AlertStatePill state={rule.state.state} />
			</div>
			<div><span class="label">Condition</span> {rule.condition_type} — {conditionSummary(rule)}</div>
			<div><span class="label">Query</span> <code>{rule.query}</code></div>
			<div><span class="label">Evaluation interval</span> {rule.eval_interval_seconds}s</div>
			<div><span class="label">Debounce (for)</span> {rule.for_minutes}m</div>
			<div><span class="label">Enabled</span> {rule.enabled ? 'yes' : 'no'}</div>
			{#if rule.state.last_eval_status === 'error'}
				<div class="eval-error">
					<span class="label">Last evaluation error</span> {rule.state.last_error}
					({rule.state.consecutive_errors} consecutive)
				</div>
			{:else if rule.state.last_value !== undefined}
				<div><span class="label">Last observed value</span> {rule.state.last_value}</div>
			{/if}
		</section>

		<h2>State history</h2>
		<p class="hint">Most recent first — this is "why didn't I get paged."</p>
		<DeliveryTimeline {deliveries} />
	{/if}
</main>

<style>
	main {
		max-width: 56rem;
	}
	.header {
		display: flex;
		align-items: center;
		justify-content: space-between;
	}
	.desc {
		color: var(--color-text-muted);
	}
	.error {
		color: var(--color-danger);
	}
	.summary {
		border: 1px solid var(--color-border);
		border-radius: var(--radius-md);
		padding: var(--space-3) var(--space-4);
		margin: var(--space-4) 0;
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
		font-size: var(--text-base);
		background: var(--color-surface);
	}
	.label {
		font-weight: var(--font-weight-medium);
		margin-right: var(--space-1);
	}
	.eval-error {
		color: var(--color-danger);
	}
	.hint {
		font-size: var(--text-sm);
		color: var(--color-text-muted);
	}
</style>
