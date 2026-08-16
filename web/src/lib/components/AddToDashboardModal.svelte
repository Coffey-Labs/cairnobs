<script lang="ts">
	// The Search page's "add as panel" affordance -- any query result can
	// become a dashboard panel without leaving the query bar. Reuses the
	// same addPanel call PanelEditor uses; this modal is deliberately
	// simpler than PanelEditor (no live preview -- the result the user is
	// looking at *is* the preview) since its only job is picking a
	// destination and a chart type for a query that's already been run.
	import { Modal, Button, Select } from '$lib/components/ui';
	import { listDashboards, getDashboard, addPanel, type Dashboard, type VizType, type Language } from '$lib/api';

	let {
		open = $bindable(false),
		query,
		language
	}: { open?: boolean; query: string; language: Language } = $props();

	let dashboards = $state<Dashboard[]>([]);
	let loadingDashboards = $state(false);
	let targetId = $state('');
	let title = $state('');
	let vizType = $state<VizType>('table');
	let saving = $state(false);
	let error = $state('');
	let done = $state(false);

	$effect(() => {
		if (!open) {
			done = false;
			error = '';
			return;
		}
		loadingDashboards = true;
		listDashboards()
			.then((d) => {
				dashboards = d;
				if (d.length && !targetId) targetId = d[0].id;
			})
			.catch((e) => (error = e instanceof Error ? e.message : String(e)))
			.finally(() => (loadingDashboards = false));
	});

	async function save() {
		if (!targetId) return;
		saving = true;
		error = '';
		try {
			const target = await getDashboard(targetId);
			const nextY = target.panels?.length ? Math.max(...target.panels.map((p) => p.position_y + p.height)) : 0;
			await addPanel(targetId, {
				title: title || query,
				query,
				query_language: language,
				viz_type: vizType,
				position_x: 0,
				position_y: nextY,
				width: 6,
				height: 4
			});
			done = true;
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		} finally {
			saving = false;
		}
	}

	const vizOptions: { value: VizType; label: string }[] = [
		{ value: 'table', label: 'Table' },
		{ value: 'line', label: 'Line chart' },
		{ value: 'bar', label: 'Bar chart' },
		{ value: 'single_stat', label: 'Single stat' },
		{ value: 'top_n', label: 'Top-N' },
		{ value: 'heatmap', label: 'Heatmap' }
	];
</script>

<Modal bind:open title="Add to dashboard">
	{#if done}
		<p>Added. <a href="/dashboards/{targetId}">Open the dashboard</a> to arrange it.</p>
	{:else if loadingDashboards}
		<p class="muted">Loading dashboards…</p>
	{:else if dashboards.length === 0}
		<p class="muted">No dashboards yet — <a href="/dashboards">create one first</a>.</p>
	{:else}
		<div class="form">
			<label class="field">
				Dashboard
				<Select bind:value={targetId}>
					{#each dashboards as d (d.id)}
						<option value={d.id}>{d.name}</option>
					{/each}
				</Select>
			</label>
			<label class="field">
				Panel title (optional)
				<input placeholder={query} bind:value={title} />
			</label>
			<label class="field">
				Visualization
				<Select bind:value={vizType}>
					{#each vizOptions as opt (opt.value)}
						<option value={opt.value}>{opt.label}</option>
					{/each}
				</Select>
			</label>
			{#if error}<p class="error">{error}</p>{/if}
		</div>
	{/if}

	{#snippet footer()}
		{#if done}
			<Button variant="primary" onclick={() => (open = false)}>Close</Button>
		{:else}
			<Button variant="ghost" onclick={() => (open = false)}>Cancel</Button>
			<Button variant="primary" onclick={save} disabled={saving || !targetId}>
				{saving ? 'Adding…' : 'Add panel'}
			</Button>
		{/if}
	{/snippet}
</Modal>

<style>
	.form {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
	}
	.field {
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
		font-size: var(--text-sm);
		color: var(--color-text-muted);
	}
	.field input {
		background: var(--color-bg);
		color: var(--color-text);
		border: 1px solid var(--color-border);
		border-radius: var(--radius-sm);
		padding: var(--space-2);
		font-family: var(--font-ui);
	}
	.muted {
		color: var(--color-text-muted);
	}
	.error {
		color: var(--color-danger);
	}
</style>
