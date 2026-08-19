<script lang="ts">
	import { listMetricsHosts, type HostSummary } from '$lib/api';
	import { EmptyState, Skeleton, Table } from '$lib/components/ui';

	let hosts = $state<HostSummary[]>([]);
	let loading = $state(true);
	let error = $state('');

	async function load() {
		loading = true;
		error = '';
		try {
			hosts = await listMetricsHosts();
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		} finally {
			loading = false;
		}
	}
	load();
</script>

<main>
	<h1>Hosts</h1>
	<p class="subtitle">
		CPU, memory, and disk usage for every host reporting metrics. Only one agent process per
		physical host reports these -- see agent/README.md's "Host CPU/memory/disk metrics" section.
	</p>
	{#if error}<p class="error">Error: {error}</p>{/if}

	{#if loading}
		<div class="skeleton-list">
			{#each Array(3) as _, i (i)}
				<Skeleton height="2.25rem" />
			{/each}
		</div>
	{:else if hosts.length === 0}
		<EmptyState
			icon="▣"
			title="No hosts reporting metrics yet"
			description="A host appears here once an agent with [metrics] enabled = true has sent its first sample -- see agent/README.md."
		/>
	{:else}
		<Table>
			<thead>
				<tr>
					<th>Host</th>
				</tr>
			</thead>
			<tbody>
				{#each hosts as h (h.host)}
					<tr>
						<td><a href={`/hosts/${encodeURIComponent(h.host)}`}>{h.host}</a></td>
					</tr>
				{/each}
			</tbody>
		</Table>
	{/if}
</main>

<style>
	main {
		max-width: 56rem;
	}
	h1 {
		font-size: var(--text-xl);
		margin-bottom: var(--space-2);
	}
	.subtitle {
		color: var(--color-text-muted);
		font-size: var(--text-sm);
		margin-bottom: var(--space-5);
	}
	.error {
		color: var(--color-danger);
	}
	.skeleton-list {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}
	a {
		color: var(--color-text);
		font-weight: var(--font-weight-medium);
		text-decoration: none;
	}
	a:hover {
		color: var(--color-accent);
	}
</style>
