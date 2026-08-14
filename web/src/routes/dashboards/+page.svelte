<script lang="ts">
	import {
		listDashboards,
		createDashboard,
		deleteDashboard,
		importDashboard,
		type Dashboard
	} from '$lib/api';

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
		<input
			placeholder="New dashboard name"
			bind:value={newName}
			onkeydown={(e) => e.key === 'Enter' && create()}
		/>
		<button onclick={create} disabled={!newName.trim()}>Create</button>
		<label class="import-label">
			Import JSON
			<input type="file" accept="application/json" onchange={onImportFile} hidden />
		</label>
	</div>

	{#if loading}
		<p>Loading…</p>
	{:else if dashboards.length === 0}
		<p>No dashboards yet.</p>
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
		font-family: system-ui, sans-serif;
		max-width: 960px;
		margin: 2rem auto;
		padding: 0 1rem;
	}
	.error {
		color: #b00020;
	}
	.create-row {
		display: flex;
		align-items: center;
		gap: 0.5rem;
		margin-bottom: 1.5rem;
	}
	.import-label {
		cursor: pointer;
		color: #06c;
		font-size: 0.85rem;
	}
	.dashboard-list {
		list-style: none;
		padding: 0;
	}
	.dashboard-list li {
		display: flex;
		align-items: center;
		gap: 0.75rem;
		padding: 0.5rem 0;
		border-bottom: 1px solid #eee;
	}
	.dashboard-list a {
		font-weight: 600;
		color: #06c;
		text-decoration: none;
	}
	.desc {
		color: #777;
		font-size: 0.85rem;
	}
	.delete {
		margin-left: auto;
		color: #b00020;
		background: none;
		border: 1px solid #b00020;
		border-radius: 4px;
		padding: 0.15rem 0.5rem;
		cursor: pointer;
	}
</style>
