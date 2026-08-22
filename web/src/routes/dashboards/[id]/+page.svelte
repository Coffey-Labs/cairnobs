<script lang="ts">
	import { getTimezone } from '$lib/timezone.svelte';
	import { toQueryTimeValue } from '$lib/querytime';
	import { page } from '$app/state';
	import { GridStack, type GridStackNode } from 'gridstack';
	import 'gridstack/dist/gridstack.min.css';
	import PanelViz from '$lib/PanelViz.svelte';
	import PanelEditor from '$lib/components/PanelEditor.svelte';
	import { Button, Card, EmptyState, Skeleton } from '$lib/components/ui';
	import {
		getDashboard,
		updateDashboard,
		deleteDashboard as apiDeleteDashboard,
		deletePanel as apiDeletePanel,
		updatePanel as apiUpdatePanel,
		exportDashboard,
		runQuery,
		resolveTimeRange,
		injectTimeRange,
		type Dashboard,
		type Panel,
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

	let editorOpen = $state(false);
	let editingPanel = $state<Panel | null>(null);

	function openNewPanel() {
		editingPanel = null;
		editorOpen = true;
	}
	function openEditPanel(panel: Panel) {
		editingPanel = panel;
		editorOpen = true;
	}

	// Zoom on a time-series panel becomes the dashboard's new global
	// range -- the brief's "zoomed range able to feed back into the
	// global dashboard time-range picker" requirement. Reuses the exact
	// same applyTimeRange() path the manual earliest/latest inputs use,
	// so a zoom and a typed range behave identically (persisted, re-runs
	// every panel), not two divergent code paths.
	async function onPanelZoom(range: { earliest: string; latest: string }) {
		earliestInput = range.earliest;
		latestInput = range.latest;
		await applyTimeRange();
	}

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
			const result = await runQuery(
				injectTimeRange(panel.query, earliest, latest, getTimezone()),
				panel.query_language
			);
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
		// Resolve a typed wall-clock time to an explicit UTC instant
		// *before* saving, not when a panel runs. A dashboard is shared:
		// storing "2026-08-22 10:00" would mean 10am in whatever zone each
		// viewer happens to be in, so two people would see two different
		// windows of data on the same dashboard. Storing the instant it
		// meant to its author keeps one dashboard showing one thing.
		// Relative ranges (-24h) are untouched and stay relative.
		earliestInput = resolveTypedTime(earliestInput, getTimezone());
		latestInput = resolveTypedTime(latestInput, getTimezone());
		dashboard = await updateDashboard(dashboardId, {
			name: dashboard.name,
			description: dashboard.description,
			default_earliest: earliestInput,
			default_latest: latestInput
		});
		await runAllPanels();
	}

	// Same conversion the query path uses, minus the quoting -- these two
	// fields are stored as bare values and quoted later by
	// injectTimeRange.
	function resolveTypedTime(value: string, zone: string): string {
		return toQueryTimeValue(value, zone).replace(/^"|"$/g, '');
	}

	function nextY(): number {
		if (!dashboard?.panels || dashboard.panels.length === 0) return 0;
		return Math.max(...dashboard.panels.map((p) => p.position_y + p.height));
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
		<div class="skeleton-header">
			<Skeleton width="16rem" height="1.75rem" />
			<Skeleton width="8rem" height="1.5rem" />
		</div>
		<div class="skeleton-grid">
			{#each Array(4) as _, i (i)}
				<Card><Skeleton height="10rem" /></Card>
			{/each}
		</div>
	{:else if !dashboard}
		<EmptyState icon="⚠" title="Couldn't load this dashboard" description={error} />
	{:else}
		<div class="header">
			<h1>{dashboard.name}</h1>
			<div class="header-actions">
				<Button variant="secondary" onclick={doExport}>Export JSON</Button>
				<Button variant="danger" onclick={removeDashboard}>Delete dashboard</Button>
			</div>
		</div>
		{#if dashboard.description}<p class="desc">{dashboard.description}</p>{/if}
		{#if error}<p class="error">Error: {error}</p>{/if}

		<div class="time-range">
			<label>Earliest <input bind:value={earliestInput} placeholder="-1h" /></label>
			<label>Latest <input bind:value={latestInput} placeholder="now" /></label>
			<Button size="sm" onclick={applyTimeRange}>Apply to all panels</Button>
			<span class="hint">Per-panel overrides win over this default, and a time-series panel's zoom updates this automatically.</span>
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
								<button class="panel-title" onclick={() => openEditPanel(panel)} title="Edit panel">
									{panel.title || panel.query}
								</button>
								<button class="panel-delete" onclick={() => removePanel(panel.id)} aria-label="Delete panel">×</button>
							</div>
							{#if panelErrors[panel.id]}
								<p class="error">Error: {panelErrors[panel.id]}</p>
							{:else if panelResults[panel.id]}
								<PanelViz
									result={panelResults[panel.id]}
									vizType={panel.viz_type}
									vizConfig={panel.viz_config}
									query={panel.query}
									onZoom={onPanelZoom}
								/>
							{:else}
								<Skeleton height="100%" />
							{/if}
						</div>
					</div>
				{/each}
			</div>
		{:else}
			<EmptyState
				icon="▤"
				title="No panels yet"
				description="Build a query on the Search page, then add it here — or start straight from a blank panel below."
			>
				{#snippet action()}
					<Button variant="primary" onclick={openNewPanel}>+ Add your first panel</Button>
				{/snippet}
			</EmptyState>
		{/if}

		{#if dashboard.panels && dashboard.panels.length > 0}
			<div class="add-panel">
				<Button onclick={openNewPanel}>+ Add panel</Button>
			</div>
		{/if}

		<PanelEditor
			bind:open={editorOpen}
			{dashboardId}
			panel={editingPanel}
			dashboardEarliest={earliestInput}
			dashboardLatest={latestInput}
			{nextY}
			onSaved={load}
		/>
	{/if}
</main>

<style>
	main {
		max-width: 75rem;
	}
	.header {
		display: flex;
		align-items: center;
		justify-content: space-between;
	}
	.header-actions {
		display: flex;
		gap: var(--space-2);
	}
	.desc {
		color: var(--color-text-muted);
	}
	.error {
		color: var(--color-danger);
	}
	.time-range {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		margin: var(--space-4) 0;
		flex-wrap: wrap;
	}
	.time-range input {
		width: 6rem;
		background: var(--color-surface);
		color: var(--color-text);
		border: 1px solid var(--color-border);
		border-radius: var(--radius-sm);
		padding: var(--space-1) var(--space-2);
		font-family: var(--font-mono);
	}
	.hint {
		font-size: var(--text-sm);
		color: var(--color-text-muted);
	}
	.panel {
		border: 1px solid var(--color-border);
		border-radius: var(--radius-md);
		padding: var(--space-2) var(--space-3);
		height: 100%;
		box-sizing: border-box;
		overflow: auto;
		background: var(--color-surface);
		color: var(--color-text);
	}
	.panel-header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-2);
		margin-bottom: var(--space-2);
	}
	.panel-title {
		background: none;
		border: none;
		padding: 0;
		font-family: var(--font-ui);
		font-weight: var(--font-weight-medium);
		font-size: var(--text-base);
		color: var(--color-text);
		text-align: left;
		cursor: pointer;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.panel-title:hover {
		color: var(--color-accent);
	}
	.panel-delete {
		background: none;
		border: none;
		cursor: pointer;
		font-size: var(--text-md);
		color: var(--color-text-muted);
		flex: none;
	}
	.panel-delete:hover {
		color: var(--color-danger);
	}
	.add-panel {
		margin-top: var(--space-5);
	}
	.skeleton-header {
		display: flex;
		justify-content: space-between;
		margin-bottom: var(--space-5);
	}
	.skeleton-grid {
		display: grid;
		grid-template-columns: repeat(auto-fit, minmax(20rem, 1fr));
		gap: var(--space-4);
	}
</style>
