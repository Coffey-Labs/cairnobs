<script lang="ts">
	import { page } from '$app/state';
	import { getRule, deleteRule, listDeliveries, type AlertRule, type DeliveryLogEntry } from '$lib/api';

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
			<button class="delete" onclick={remove}>Delete rule</button>
		</div>
		{#if rule.description}<p class="desc">{rule.description}</p>{/if}
		{#if error}<p class="error">Error: {error}</p>{/if}

		<section class="summary">
			<div>
				<span class="label">State</span>
				<span
					class="state"
					class:firing={rule.state.state === 'firing'}
					class:pending={rule.state.state === 'pending'}
				>
					{rule.state.state}
				</span>
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

		<h2>Delivery log</h2>
		<p class="hint">Most recent first — this is "why didn't I get paged."</p>
		{#if deliveries.length === 0}
			<p>No deliveries yet.</p>
		{:else}
			<table>
				<thead>
					<tr>
						<th>When</th>
						<th>Event</th>
						<th>Status</th>
						<th>Attempts</th>
						<th>Response</th>
						<th>Error</th>
					</tr>
				</thead>
				<tbody>
					{#each deliveries as d (d.id)}
						<tr>
							<td>{new Date(d.created_at).toLocaleString()}</td>
							<td>{d.event_type}</td>
							<td>{d.status}</td>
							<td>{d.attempt_count}</td>
							<td>{d.response_status ?? '—'}</td>
							<td class="error-cell">{d.last_error ?? ''}</td>
						</tr>
					{/each}
				</tbody>
			</table>
		{/if}
	{/if}
</main>

<style>
	main {
		font-family: system-ui, sans-serif;
		max-width: 900px;
		margin: 2rem auto;
		padding: 0 1rem;
	}
	.header {
		display: flex;
		align-items: center;
		justify-content: space-between;
	}
	.desc {
		color: #555;
	}
	.error {
		color: #b00020;
	}
	.summary {
		border: 1px solid #ddd;
		border-radius: 6px;
		padding: 0.75rem 1rem;
		margin: 1rem 0;
		display: flex;
		flex-direction: column;
		gap: 0.4rem;
		font-size: 0.9rem;
	}
	.label {
		font-weight: 600;
		margin-right: 0.4rem;
	}
	.state {
		font-size: 0.75rem;
		padding: 0.1rem 0.5rem;
		border-radius: 1rem;
		background: #eee;
	}
	.state.pending {
		background: #ffe9b3;
	}
	.state.firing {
		background: #fdd;
		color: #900;
	}
	.eval-error {
		color: #b00020;
	}
	.hint {
		font-size: 0.8rem;
		color: #777;
	}
	.delete {
		color: #b00020;
		background: none;
		border: 1px solid #b00020;
		border-radius: 4px;
		padding: 0.15rem 0.5rem;
		cursor: pointer;
	}
	table {
		border-collapse: collapse;
		width: 100%;
	}
	th,
	td {
		border-bottom: 1px solid #eee;
		padding: 0.3rem 0.5rem;
		text-align: left;
		font-size: 0.85rem;
	}
	.error-cell {
		color: #b00020;
		max-width: 20rem;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
</style>
