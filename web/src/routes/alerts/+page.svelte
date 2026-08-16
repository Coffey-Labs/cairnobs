<script lang="ts">
	import { listRules, deleteRule, type AlertRule } from '$lib/api';
	import { Button, Table, EmptyState, Skeleton } from '$lib/components/ui';
	import AlertStatePill from '$lib/components/AlertStatePill.svelte';

	let rules = $state<AlertRule[]>([]);
	let loading = $state(true);
	let error = $state('');

	async function load() {
		loading = true;
		try {
			rules = await listRules();
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		} finally {
			loading = false;
		}
	}
	load();

	async function remove(id: string) {
		try {
			await deleteRule(id);
			await load();
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		}
	}

	function conditionSummary(r: AlertRule): string {
		if (r.condition_type === 'absence') return 'absence (query returns zero rows)';
		const symbols: Record<string, string> = { gt: '>', gte: '>=', lt: '<', lte: '<=', eq: '==', ne: '!=' };
		return `${symbols[r.comparator ?? ''] ?? r.comparator} ${r.threshold_value}`;
	}

	// Firing rules first, then pending, then ok -- "what needs my
	// attention" reads at the top of the list without having to scan
	// every row (Phase 5 task 7's "at a glance" requirement).
	const statePriority: Record<AlertRule['state']['state'], number> = { firing: 0, pending: 1, ok: 2 };
	let sortedRules = $derived([...rules].sort((a, b) => statePriority[a.state.state] - statePriority[b.state.state]));
</script>

<main>
	<div class="header">
		<h1>Alerts</h1>
		<Button href="/alerts/new" variant="primary">+ New rule</Button>
	</div>
	{#if error}<p class="error">Error: {error}</p>{/if}

	{#if loading}
		<div class="skeleton-list">
			{#each Array(3) as _, i (i)}
				<Skeleton height="2.5rem" />
			{/each}
		</div>
	{:else if rules.length === 0}
		<EmptyState
			icon="▲"
			title="No alert rules yet"
			description="Rules watch a query on an interval and notify you when it crosses a threshold, or goes silent."
		>
			{#snippet action()}
				<Button href="/alerts/new" variant="primary">+ New rule</Button>
			{/snippet}
		</EmptyState>
	{:else}
		<Table>
			<thead>
				<tr>
					<th>State</th>
					<th>Name</th>
					<th>Condition</th>
					<th>Enabled</th>
					<th><span class="sr-only">Actions</span></th>
				</tr>
			</thead>
			<tbody>
				{#each sortedRules as r (r.id)}
					<tr>
						<td>
							<AlertStatePill state={r.state.state} />
							{#if r.state.last_eval_status === 'error'}
								<span class="eval-error" title={r.state.last_error}>eval error</span>
							{/if}
						</td>
						<td><a href={`/alerts/${r.id}`}>{r.name}</a></td>
						<td><code>{conditionSummary(r)}</code></td>
						<td>{r.enabled ? 'yes' : 'no'}</td>
						<td><Button size="sm" variant="danger" onclick={() => remove(r.id)}>Delete</Button></td>
					</tr>
				{/each}
			</tbody>
		</Table>
	{/if}
</main>

<style>
	main {
		max-width: 60rem;
	}
	.header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		margin-bottom: var(--space-4);
	}
	h1 {
		font-size: var(--text-xl);
	}
	.error {
		color: var(--color-danger);
	}
	.skeleton-list {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}
	.eval-error {
		margin-left: var(--space-2);
		font-size: var(--text-xs);
		color: var(--color-danger);
	}
</style>
