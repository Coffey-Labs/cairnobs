<script lang="ts">
	import { listAgents, type Agent } from '$lib/api';
	import { Badge, EmptyState, Skeleton, Table } from '$lib/components/ui';

	let agents = $state<Agent[]>([]);
	let loading = $state(true);
	let error = $state('');

	async function load() {
		loading = true;
		try {
			agents = await listAgents();
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		} finally {
			loading = false;
		}
	}
	load();

	// A host is "stale" once it's gone quiet for longer than a few of its
	// own heartbeat intervals -- 3x, with a 5-minute floor for an agent
	// that's never reported a heartbeat interval at all (heartbeat_
	// interval_ms == 0, e.g. one built before this feature existed).
	// This is a client-side display heuristic only, distinct from and
	// looser than the real alerting mechanism -- see
	// /docs/agent-heartbeat-monitoring.md for the actual absence-alert-
	// rule-based detection this page doesn't replace.
	function isStale(a: Agent): boolean {
		const thresholdMs = Math.max(a.heartbeat_interval_ms * 3, 5 * 60 * 1000);
		return Date.now() - new Date(a.last_seen_at).getTime() > thresholdMs;
	}

	function relativeTime(iso: string): string {
		const ms = Date.now() - new Date(iso).getTime();
		if (ms < 60_000) return `${Math.max(0, Math.round(ms / 1000))}s ago`;
		if (ms < 3_600_000) return `${Math.round(ms / 60_000)}m ago`;
		if (ms < 86_400_000) return `${Math.round(ms / 3_600_000)}h ago`;
		return `${Math.round(ms / 86_400_000)}d ago`;
	}
</script>

<main>
	<h1>Agents</h1>
	<p class="subtitle">
		Linux/Windows log collection agents that have checked in at least once.
	</p>
	{#if error}<p class="error">Error: {error}</p>{/if}

	{#if loading}
		<div class="skeleton-list">
			{#each Array(3) as _, i (i)}
				<Skeleton height="2.25rem" />
			{/each}
		</div>
	{:else if agents.length === 0}
		<EmptyState
			icon="●"
			title="No agents have checked in yet"
			description="An agent appears here automatically the first time it successfully calls in to ingest -- there's no manual registration step. See the agent README for how to point one at this deployment."
		/>
	{:else}
		<Table>
			<thead>
				<tr>
					<th>Host</th>
					<th>Service</th>
					<th>Version</th>
					<th>Last seen</th>
					<th>Status</th>
					<th>Config</th>
				</tr>
			</thead>
			<tbody>
				{#each agents as a (a.id)}
					<tr>
						<td><a href={`/agents/${encodeURIComponent(a.host)}`}>{a.host}</a></td>
						<td>{a.service}</td>
						<td>{a.agent_version || '—'}</td>
						<td>{relativeTime(a.last_seen_at)}</td>
						<td>
							{#if isStale(a)}
								<Badge tone="danger">stale</Badge>
							{:else}
								<Badge tone="success">healthy</Badge>
							{/if}
						</td>
						<td>
							{#if a.pending}
								<Badge tone="accent">pending</Badge>
							{:else if a.desired_override}
								<Badge tone="neutral">applied</Badge>
							{/if}
						</td>
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
