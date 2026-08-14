<script lang="ts">
	import { listRules, deleteRule, type AlertRule } from '$lib/api';

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
</script>

<main>
	<h1>Alerts</h1>
	{#if error}<p class="error">Error: {error}</p>{/if}

	<a class="new-rule" href="/alerts/new">+ New rule</a>

	{#if loading}
		<p>Loading…</p>
	{:else if rules.length === 0}
		<p>No alert rules yet.</p>
	{:else}
		<table>
			<thead>
				<tr>
					<th>Name</th>
					<th>Condition</th>
					<th>State</th>
					<th>Enabled</th>
					<th></th>
				</tr>
			</thead>
			<tbody>
				{#each rules as r (r.id)}
					<tr>
						<td><a href={`/alerts/${r.id}`}>{r.name}</a></td>
						<td><code>{conditionSummary(r)}</code></td>
						<td>
							<span class="state" class:firing={r.state.state === 'firing'} class:pending={r.state.state === 'pending'}>
								{r.state.state}
							</span>
							{#if r.state.last_eval_status === 'error'}
								<span class="eval-error" title={r.state.last_error}>eval error</span>
							{/if}
						</td>
						<td>{r.enabled ? 'yes' : 'no'}</td>
						<td><button class="delete" onclick={() => remove(r.id)}>Delete</button></td>
					</tr>
				{/each}
			</tbody>
		</table>
	{/if}
</main>

<style>
	main {
		font-family: system-ui, sans-serif;
		max-width: 960px;
		margin: 2rem auto;
		padding: 0 1rem;
	}
	.error {
		color: #b00020;
	}
	.new-rule {
		display: inline-block;
		margin-bottom: 1rem;
		color: #06c;
		text-decoration: none;
	}
	table {
		border-collapse: collapse;
		width: 100%;
	}
	th,
	td {
		border-bottom: 1px solid #eee;
		padding: 0.4rem 0.6rem;
		text-align: left;
		font-size: 0.9rem;
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
		margin-left: 0.4rem;
		font-size: 0.75rem;
		color: #b00020;
	}
	.delete {
		color: #b00020;
		background: none;
		border: 1px solid #b00020;
		border-radius: 4px;
		padding: 0.15rem 0.5rem;
		cursor: pointer;
	}
</style>
