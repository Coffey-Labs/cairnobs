<script lang="ts">
	import {
		listDashboards,
		createDashboard,
		deleteDashboard,
		importDashboard,
		type Dashboard
	} from '$lib/api';
	import { Button, Input, EmptyState, Skeleton } from '$lib/components/ui';

	let dashboards = $state<Dashboard[]>([]);
	let loading = $state(true);
	let error = $state('');
	let newName = $state('');

	async function load() {
		loading = true;
		try {
			dashboards = await listDashboards();
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		} finally {
			loading = false;
		}
	}
	load();

	async function create() {
		if (!newName.trim()) return;
		try {
			await createDashboard({ name: newName.trim() });
			newName = '';
			await load();
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		}
	}

	async function remove(id: string) {
		try {
			await deleteDashboard(id);
			await load();
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		}
	}

	async function onImportFile(e: Event) {
		const input = e.target as HTMLInputElement;
		const file = input.files?.[0];
		if (!file) return;
		try {
			const text = await file.text();
			await importDashboard(JSON.parse(text));
			await load();
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		} finally {
			input.value = '';
		}
	}
</script>

<main>
	<h1>Dashboards</h1>
	{#if error}<p class="error">Error: {error}</p>{/if}

	<div class="create-row">
		<Input
			placeholder="New dashboard name"
			bind:value={newName}
			onkeydown={(e: KeyboardEvent) => e.key === 'Enter' && create()}
		/>
		<Button onclick={create} disabled={!newName.trim()}>Create</Button>
		<label class="import-label">
			Import JSON
			<input type="file" accept="application/json" onchange={onImportFile} hidden />
		</label>
	</div>

	{#if loading}
		<div class="skeleton-list">
			{#each Array(3) as _, i (i)}
				<Skeleton height="2.25rem" />
			{/each}
		</div>
	{:else if dashboards.length === 0}
		<EmptyState
			icon="▤"
			title="No dashboards yet"
			description="Build a query on the Search page and save it here, or create an empty dashboard above and add panels to it."
		/>
	{:else}
		<ul class="dashboard-list">
			{#each dashboards as d (d.id)}
				<li>
					<a href={`/dashboards/${d.id}`}>{d.name}</a>
					{#if d.description}<span class="desc">{d.description}</span>{/if}
					<button class="delete" onclick={() => remove(d.id)}>Delete</button>
				</li>
			{/each}
		</ul>
	{/if}
</main>

<style>
	main {
		max-width: 48rem;
	}
	h1 {
		font-size: var(--text-xl);
		margin-bottom: var(--space-4);
	}
	.error {
		color: var(--color-danger);
	}
	.skeleton-list {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}
	.create-row {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		margin-bottom: var(--space-5);
	}
	.import-label {
		cursor: pointer;
		color: var(--color-accent);
		font-size: var(--text-sm);
		white-space: nowrap;
	}
	.dashboard-list {
		list-style: none;
		padding: 0;
	}
	.dashboard-list li {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		padding: var(--space-3) 0;
		border-bottom: 1px solid var(--color-border);
	}
	.dashboard-list a {
		font-weight: var(--font-weight-medium);
		color: var(--color-text);
		text-decoration: none;
	}
	.dashboard-list a:hover {
		color: var(--color-accent);
	}
	.desc {
		color: var(--color-text-muted);
		font-size: var(--text-sm);
	}
	.delete {
		margin-left: auto;
		color: var(--color-danger);
		background: none;
		border: 1px solid var(--color-danger);
		border-radius: var(--radius-sm);
		padding: 0.15rem var(--space-2);
		cursor: pointer;
		font-family: var(--font-ui);
	}
</style>
