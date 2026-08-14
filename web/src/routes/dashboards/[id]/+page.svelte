<script lang="ts">
	import { page } from '$app/state';
	import { GridStack, type GridStackNode } from 'gridstack';
	import 'gridstack/dist/gridstack.min.css';
	import QueryBar from '$lib/QueryBar.svelte';
	import PanelViz from '$lib/PanelViz.svelte';
	import {
		getDashboard,
		updateDashboard,
		deleteDashboard as apiDeleteDashboard,
		addPanel,
		deletePanel as apiDeletePanel,
		updatePanel as apiUpdatePanel,
		exportDashboard,
		runQuery,
		resolveTimeRange,
		injectTimeRange,
		type Dashboard,
		type Panel,
		type VizType,
		type Language,
		type QueryResult
	} from '$lib/api';

	const dashboardId = page.params.id!;

	let dashboard = $state<Dashboard | null>(null);
	let loading = $state(true);
	let error = $state('');

	let earliestInput = $state('-1h');
	let latestInput = $state('now');

	let panelResults = $state<Record<string, QueryResult>>({});
	let panelErrors = $state<Record<string, string>>({});

	let gridEl: HTMLDivElement | undefined = $state();
	let grid: GridStack | undefined;

	let showAddPanel = $state(false);
	let newTitle = $state('');
	let newQuery = $state('');
	let newLanguage = $state<Language>('');
	let newVizType = $state<VizType>('table');

	async function load() {
		loading = true;
		error = '';
		try {
			dashboard = await getDashboard(dashboardId);
			earliestInput = dashboard.default_earliest;
			latestInput = dashboard.default_latest;
			await runAllPanels();
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		} finally {
			loading = false;
		}
	}
	load();

	async function runPanel(panel: Panel) {
		if (!dashboard) return;
		const { earliest, latest } = resolveTimeRange(dashboard, panel);
		try {
			const result = await runQuery(injectTimeRange(panel.query, earliest, latest), panel.query_language);
			panelResults = { ...panelResults, [panel.id]: result };
			if (panelErrors[panel.id]) {
				const rest = { ...panelErrors };
				delete rest[panel.id];
				panelErrors = rest;
			}
		} catch (e) {
			// A broken panel query shouldn't take down the rest of the
			// dashboard -- per /docs/phase-3-dashboard-design.md's "panel
			// execution" section, panels load and error independently.
			panelErrors = { ...panelErrors, [panel.id]: e instanceof Error ? e.message : String(e) };
		}
	}

	async function runAllPanels() {
		if (!dashboard?.panels) return;
		await Promise.all(dashboard.panels.map(runPanel));
	}

	async function applyTimeRange() {
		if (!dashboard) return;
		dashboard = await updateDashboard(dashboardId, {
			name: dashboard.name,
			description: dashboard.description,
			default_earliest: earliestInput,
			default_latest: latestInput
		});
		await runAllPanels();
	}

	function nextY(): number {
		if (!dashboard?.panels || dashboard.panels.length === 0) return 0;
		return Math.max(...dashboard.panels.map((p) => p.position_y + p.height));
	}

	async function submitAddPanel() {
		if (!newQuery.trim()) return;
		try {
			await addPanel(dashboardId, {
				title: newTitle,
				query: newQuery,
				query_language: newLanguage,
				viz_type: newVizType,
				position_x: 0,
				position_y: nextY(),
				width: 6,
				height: 4
			});
			showAddPanel = false;
			newTitle = '';
			newQuery = '';
			newLanguage = '';
			newVizType = 'table';
			await load();
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		}
	}

	async function removePanel(panelId: string) {
		try {
			await apiDeletePanel(dashboardId, panelId);
			await load();
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		}
	}

	async function removeDashboard() {
		await apiDeleteDashboard(dashboardId);
		window.location.href = '/dashboards';
	}

	async function doExport() {
		const doc = await exportDashboard(dashboardId);
		const blob = new Blob([JSON.stringify(doc, null, 2)], { type: 'application/json' });
		const url = URL.createObjectURL(blob);
		const a = document.createElement('a');
		a.href = url;
		a.download = `${doc.name.replace(/\s+/g, '-').toLowerCase() || 'dashboard'}.json`;
		a.click();
		URL.revokeObjectURL(url);
	}

	// Persists drag/resize moves back to the API. Re-initialized whenever
	// the *set* of panels changes (add/remove/reload) -- not on every
	// result update, which would disrupt an in-progress drag.
	function setupGrid() {
		if (!gridEl || !dashboard?.panels) return;
		grid?.destroy(false);
		grid = GridStack.init({ float: true, cellHeight: 60, column: 12 }, gridEl);
		grid.on('change', (_event: Event, items: GridStackNode[]) => {
			for (const item of items) {
				const panel = dashboard?.panels?.find((p) => p.id === item.id);
				if (!panel) continue;
				apiUpdatePanel(dashboardId, {
					...panel,
					position_x: item.x ?? panel.position_x,
					position_y: item.y ?? panel.position_y,
					width: item.w ?? panel.width,
					height: item.h ?? panel.height
				}).catch(() => {
					// Best-effort persistence -- a failed position save isn't
					// worth surfacing as a page-level error; the layout is
					// still usable for the current session either way.
				});
			}
		});
	}

	let panelIds = $derived(dashboard?.panels?.map((p) => p.id).join(',') ?? '');
	$effect(() => {
		// Deliberately no queueMicrotask here: $effect in Svelte 5 already
		// runs after the DOM has committed the render that triggered it
		// (unlike $effect.pre), so the {#each} block's grid-stack-item
		// elements already exist by the time this runs. An earlier version
		// wrapped this in queueMicrotask "to be safe" and that extra hop
		// raced against Svelte's own DOM-update scheduling -- GridStack.init
		// sometimes ran before or after the elements existed depending on
		// scheduling order, silently initializing against zero items.
		panelIds;
		if (dashboard?.panels && dashboard.panels.length > 0) {
			setupGrid();
		}
	});
</script>

<main>
	{#if loading}
		<p>Loading…</p>
	{:else if !dashboard}
		<p class="error">Error: {error}</p>
	{:else}
		<div class="header">
			<h1>{dashboard.name}</h1>
			<div class="header-actions">
				<button onclick={doExport}>Export JSON</button>
				<button class="delete" onclick={removeDashboard}>Delete dashboard</button>
			</div>
		</div>
		{#if dashboard.description}<p class="desc">{dashboard.description}</p>{/if}
		{#if error}<p class="error">Error: {error}</p>{/if}

		<div class="time-range">
			<label>Earliest <input bind:value={earliestInput} placeholder="-1h" /></label>
			<label>Latest <input bind:value={latestInput} placeholder="now" /></label>
			<button onclick={applyTimeRange}>Apply to all panels</button>
			<span class="hint">Per-panel overrides win over this default -- see the panel editor.</span>
		</div>

		{#if dashboard.panels && dashboard.panels.length > 0}
			<div class="grid-stack" bind:this={gridEl}>
				{#each dashboard.panels as panel (panel.id)}
					<div
						class="grid-stack-item"
						{...{
							'gs-id': panel.id,
							'gs-x': panel.position_x,
							'gs-y': panel.position_y,
							'gs-w': panel.width,
							'gs-h': panel.height
						}}
					>
						<div class="grid-stack-item-content panel">
							<div class="panel-header">
								<span class="panel-title">{panel.title || panel.query}</span>
								<button class="panel-delete" onclick={() => removePanel(panel.id)}>×</button>
							</div>
							{#if panelErrors[panel.id]}
								<p class="error">Error: {panelErrors[panel.id]}</p>
							{:else if panelResults[panel.id]}
								<PanelViz
									result={panelResults[panel.id]}
									vizType={panel.viz_type}
									vizConfig={panel.viz_config}
								/>
							{:else}
								<p>Loading…</p>
							{/if}
						</div>
					</div>
				{/each}
			</div>
		{:else}
			<p>No panels yet. Add one below.</p>
		{/if}

		<div class="add-panel">
			<button onclick={() => (showAddPanel = !showAddPanel)}>
				{showAddPanel ? 'Cancel' : '+ Add panel'}
			</button>
			{#if showAddPanel}
				<div class="add-panel-form">
					<input placeholder="Panel title" bind:value={newTitle} />
					<QueryBar bind:query={newQuery} bind:language={newLanguage} onRun={submitAddPanel} />
					<label>
						Visualization:
						<select bind:value={newVizType}>
							<option value="table">Table</option>
							<option value="line">Line chart</option>
							<option value="bar">Bar chart</option>
							<option value="single_stat">Single stat</option>
							<option value="top_n">Top-N</option>
						</select>
					</label>
					<button onclick={submitAddPanel} disabled={!newQuery.trim()}>Add panel</button>
				</div>
			{/if}
		</div>
	{/if}
</main>

<style>
	main {
		font-family: system-ui, sans-serif;
		max-width: 1200px;
		margin: 2rem auto;
		padding: 0 1rem;
	}
	.header {
		display: flex;
		align-items: center;
		justify-content: space-between;
	}
	.header-actions {
		display: flex;
		gap: 0.5rem;
	}
	.desc {
		color: #555;
	}
	.error {
		color: #b00020;
	}
	.time-range {
		display: flex;
		align-items: center;
		gap: 0.75rem;
		margin: 1rem 0;
		flex-wrap: wrap;
	}
	.time-range input {
		width: 6rem;
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
	.panel {
		border: 1px solid #ddd;
		border-radius: 6px;
		padding: 0.5rem 0.75rem;
		height: 100%;
		box-sizing: border-box;
		overflow: auto;
		background: white;
	}
	.panel-header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		font-weight: 600;
		margin-bottom: 0.5rem;
	}
	.panel-delete {
		background: none;
		border: none;
		cursor: pointer;
		font-size: 1rem;
		color: #999;
	}
	.add-panel {
		margin-top: 1.5rem;
	}
	.add-panel-form {
		border: 1px solid #ddd;
		border-radius: 6px;
		padding: 1rem;
		margin-top: 0.5rem;
		display: flex;
		flex-direction: column;
		gap: 0.75rem;
		max-width: 640px;
	}
	.add-panel-form input {
		box-sizing: border-box;
	}
</style>
