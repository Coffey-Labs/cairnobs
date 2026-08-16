<script lang="ts">
	// Add and edit share one editor: creating a panel and changing its
	// query/viz afterwards are the same form, just seeded differently and
	// calling addPanel vs updatePanel on save. The preview pane below the
	// form runs the actual query (debounced -- not on every keystroke)
	// and renders it through the real PanelViz, not a separate mock-up,
	// so what you see here is exactly what lands on the dashboard, not
	// an approximation of it.
	import { Modal, Button, Input, Select, Tabs } from '$lib/components/ui';
	import QueryBar from '$lib/QueryBar.svelte';
	import PanelViz from '$lib/PanelViz.svelte';
	import {
		runQuery,
		injectTimeRange,
		addPanel,
		updatePanel,
		type Panel,
		type VizType,
		type Language,
		type QueryResult
	} from '$lib/api';

	let {
		open = $bindable(false),
		dashboardId,
		panel = null,
		dashboardEarliest,
		dashboardLatest,
		nextY,
		onSaved
	}: {
		open?: boolean;
		dashboardId: string;
		panel?: Panel | null;
		dashboardEarliest: string;
		dashboardLatest: string;
		// Only consulted when adding a new panel -- stacks it below
		// whatever's already on the grid. The dashboard page owns panel
		// layout, so it owns this calculation too.
		nextY: () => number;
		onSaved: () => void;
	} = $props();

	let title = $state('');
	let query = $state('');
	let language = $state<Language>('');
	let vizType = $state<VizType>('table');
	let vizConfig = $state<Record<string, string>>({});
	let earliestOverride = $state('');
	let latestOverride = $state('');
	let saving = $state(false);
	let saveError = $state('');

	// Re-seed whenever the editor opens (not on every `panel` change --
	// the dashboard page keeps `panel` pointed at the same object while
	// editing, this should only reset when a *different* panel or a
	// fresh "new panel" session opens).
	let seededFor: string | null = null;
	$effect(() => {
		if (!open) {
			seededFor = null;
			return;
		}
		const key = panel?.id ?? '__new__';
		if (seededFor === key) return;
		seededFor = key;
		title = panel?.title ?? '';
		query = panel?.query ?? '';
		language = panel?.query_language ?? '';
		vizType = panel?.viz_type ?? 'table';
		vizConfig = { ...(panel?.viz_config ?? {}) };
		earliestOverride = panel?.earliest_override ?? '';
		latestOverride = panel?.latest_override ?? '';
		saveError = '';
	});

	let previewResult = $state<QueryResult | null>(null);
	let previewError = $state('');
	let previewLoading = $state(false);
	let debounceHandle: ReturnType<typeof setTimeout> | undefined;

	$effect(() => {
		// Deliberate dependency list: re-run the preview when any of these
		// change, debounced so typing a query doesn't fire a request per
		// keystroke.
		query;
		language;
		earliestOverride;
		latestOverride;
		if (!open || !query.trim()) {
			previewResult = null;
			return;
		}
		clearTimeout(debounceHandle);
		debounceHandle = setTimeout(runPreview, 400);
		return () => clearTimeout(debounceHandle);
	});

	async function runPreview() {
		previewLoading = true;
		previewError = '';
		try {
			const earliest = earliestOverride || dashboardEarliest;
			const latest = latestOverride || dashboardLatest;
			previewResult = await runQuery(injectTimeRange(query, earliest, latest), language);
		} catch (e) {
			previewError = e instanceof Error ? e.message : String(e);
			previewResult = null;
		} finally {
			previewLoading = false;
		}
	}

	async function save() {
		if (!query.trim()) return;
		saving = true;
		saveError = '';
		try {
			const input = {
				title,
				query,
				query_language: language,
				viz_type: vizType,
				viz_config: vizConfig,
				earliest_override: earliestOverride || null,
				latest_override: latestOverride || null
			};
			if (panel) {
				await updatePanel(dashboardId, { ...panel, ...input });
			} else {
				await addPanel(dashboardId, {
					...input,
					position_x: 0,
					position_y: nextY(),
					width: 6,
					height: 4
				});
			}
			open = false;
			onSaved();
		} catch (e) {
			saveError = e instanceof Error ? e.message : String(e);
		} finally {
			saving = false;
		}
	}

	function setConfig(key: string, value: string) {
		vizConfig = { ...vizConfig, [key]: value };
	}

	const vizOptions: { value: VizType; label: string }[] = [
		{ value: 'table', label: 'Table' },
		{ value: 'line', label: 'Line chart' },
		{ value: 'bar', label: 'Bar chart' },
		{ value: 'single_stat', label: 'Single stat' },
		{ value: 'top_n', label: 'Top-N' },
		{ value: 'heatmap', label: 'Heatmap' }
	];

	let tabs = [
		{ id: 'query', label: 'Query' },
		{ id: 'preview', label: 'Preview' }
	];
	let activeTab = $state('query');
</script>

<Modal bind:open title={panel ? 'Edit panel' : 'Add panel'}>
	<div class="editor">
		<Input placeholder="Panel title" bind:value={title} />

		<label class="field-label" for="viz-type">Visualization</label>
		<Select id="viz-type" bind:value={vizType}>
			{#each vizOptions as opt (opt.value)}
				<option value={opt.value}>{opt.label}</option>
			{/each}
		</Select>

		{#if vizType === 'line' || vizType === 'bar'}
			<div class="config-row">
				<Input placeholder="x column (default: 1st)" bind:value={() => vizConfig.x_column ?? '', (v) => setConfig('x_column', v)} />
				<Input
					placeholder="value column (default: 2nd)"
					bind:value={() => vizConfig.value_column ?? '', (v) => setConfig('value_column', v)}
				/>
				<Input
					placeholder="series column (optional)"
					bind:value={() => vizConfig.series_column ?? '', (v) => setConfig('series_column', v)}
				/>
			</div>
			{#if vizType === 'bar'}
				<label class="checkbox">
					<input
						type="checkbox"
						checked={vizConfig.stacked === 'true'}
						onchange={(e) => setConfig('stacked', String(e.currentTarget.checked))}
					/>
					Stack series
				</label>
			{/if}
		{:else if vizType === 'top_n'}
			<div class="config-row">
				<Input placeholder="label column (default: 1st)" bind:value={() => vizConfig.label_column ?? '', (v) => setConfig('label_column', v)} />
				<Input
					placeholder="value column (default: numeric)"
					bind:value={() => vizConfig.value_column ?? '', (v) => setConfig('value_column', v)}
				/>
			</div>
		{:else if vizType === 'heatmap'}
			<div class="config-row">
				<Input placeholder="x column (default: 1st)" bind:value={() => vizConfig.x_column ?? '', (v) => setConfig('x_column', v)} />
				<Input placeholder="y column (default: 2nd)" bind:value={() => vizConfig.y_column ?? '', (v) => setConfig('y_column', v)} />
				<Input
					placeholder="value column (default: 3rd)"
					bind:value={() => vizConfig.value_column ?? '', (v) => setConfig('value_column', v)}
				/>
			</div>
		{:else if vizType === 'single_stat'}
			<div class="config-row">
				<Input
					placeholder="value column (default: 2nd)"
					bind:value={() => vizConfig.value_column ?? '', (v) => setConfig('value_column', v)}
				/>
				<Input placeholder="unit (e.g. ms, %)" bind:value={() => vizConfig.unit ?? '', (v) => setConfig('unit', v)} />
			</div>
			<label class="checkbox">
				<input
					type="checkbox"
					checked={vizConfig.higher_is_worse === 'true'}
					onchange={(e) => setConfig('higher_is_worse', String(e.currentTarget.checked))}
				/>
				Rising trend means something is wrong (colors an increase as an error, not neutral)
			</label>
		{/if}

		<div class="overrides">
			<Input placeholder="Earliest override (e.g. -6h)" bind:value={earliestOverride} />
			<Input placeholder="Latest override (e.g. now)" bind:value={latestOverride} />
		</div>

		<Tabs {tabs} bind:active={activeTab} />

		<div id="panel-query" role="tabpanel" aria-labelledby="tab-query" hidden={activeTab !== 'query'}>
			<QueryBar bind:query bind:language onRun={runPreview} loading={previewLoading} />
		</div>

		<div id="panel-preview" role="tabpanel" aria-labelledby="tab-preview" hidden={activeTab !== 'preview'} class="preview">
			{#if previewLoading && !previewResult}
				<p class="muted">Running…</p>
			{:else if previewError}
				<p class="error">Error: {previewError}</p>
			{:else if previewResult}
				<PanelViz result={previewResult} {vizType} {vizConfig} />
			{:else}
				<p class="muted">Run a query to preview it here.</p>
			{/if}
		</div>

		{#if saveError}<p class="error">{saveError}</p>{/if}
	</div>

	{#snippet footer()}
		<Button variant="ghost" onclick={() => (open = false)}>Cancel</Button>
		<Button variant="primary" onclick={save} disabled={saving || !query.trim()}>
			{saving ? 'Saving…' : 'Save panel'}
		</Button>
	{/snippet}
</Modal>

<style>
	.editor {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
	}
	.field-label {
		font-size: var(--text-sm);
		color: var(--color-text-muted);
		margin-top: var(--space-1);
	}
	.config-row {
		display: grid;
		grid-template-columns: repeat(auto-fit, minmax(9rem, 1fr));
		gap: var(--space-2);
	}
	.overrides {
		display: grid;
		grid-template-columns: 1fr 1fr;
		gap: var(--space-2);
	}
	.checkbox {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		font-size: var(--text-sm);
		color: var(--color-text-muted);
	}
	.preview {
		min-height: 12rem;
		border: 1px solid var(--color-border);
		border-radius: var(--radius-md);
		padding: var(--space-3);
	}
	.muted {
		color: var(--color-text-muted);
	}
	.error {
		color: var(--color-danger);
	}
</style>
