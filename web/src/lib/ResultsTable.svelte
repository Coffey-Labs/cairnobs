<script lang="ts">
	// Shared between the SQL query page and the free-text search page —
	// both /api endpoints return the same {columns, rows} shape
	// specifically so this component didn't need to exist twice.
	// Phase 5: sortable columns (client-side -- the rows are already
	// fetched, re-sorting them here doesn't need a round trip), resizable
	// columns (a plain drag handle, not a dependency -- this is a small
	// enough interaction to hand-roll), and expandable rows for full
	// structured-field inspection (useful the moment a query has more
	// columns than comfortably fit, or a Map(String,String) attributes
	// column whose JSON got cut off).
	import Table from '$lib/components/ui/Table.svelte';
	import SeverityBadge from '$lib/components/ui/SeverityBadge.svelte';

	let {
		columns,
		rows,
		hasRun = false
	}: { columns: string[]; rows: unknown[][]; hasRun?: boolean } = $props();

	let severityCol = $derived(columns.indexOf('severity'));

	function formatCell(value: unknown): string {
		if (value === null || value === undefined) return '';
		if (typeof value === 'object') return JSON.stringify(value);
		return String(value);
	}

	let sortCol = $state<number | null>(null);
	let sortDir = $state<1 | -1>(1);

	function toggleSort(i: number) {
		if (sortCol === i) {
			sortDir = sortDir === 1 ? -1 : 1;
		} else {
			sortCol = i;
			sortDir = 1;
		}
	}

	let sortedRows = $derived.by(() => {
		if (sortCol === null) return rows;
		const i = sortCol;
		const dir = sortDir;
		return [...rows].sort((a, b) => {
			const av = a[i];
			const bv = b[i];
			if (typeof av === 'number' && typeof bv === 'number') return (av - bv) * dir;
			return String(av ?? '').localeCompare(String(bv ?? '')) * dir;
		});
	});

	let widths = $state<Record<number, number>>({});
	let resizing: { col: number; startX: number; startWidth: number } | null = null;

	function startResize(e: PointerEvent, i: number, currentWidth: number) {
		resizing = { col: i, startX: e.clientX, startWidth: currentWidth };
		(e.target as HTMLElement).setPointerCapture(e.pointerId);
	}
	function onResizeMove(e: PointerEvent) {
		if (!resizing) return;
		const delta = e.clientX - resizing.startX;
		widths = { ...widths, [resizing.col]: Math.max(60, resizing.startWidth + delta) };
	}
	function onResizeEnd() {
		resizing = null;
	}

	let expanded = $state<Set<number>>(new Set());
	function toggleExpanded(i: number) {
		const next = new Set(expanded);
		if (next.has(i)) next.delete(i);
		else next.add(i);
		expanded = next;
	}
</script>

{#if hasRun}
	<p class="row-count">{rows.length} row(s)</p>
{/if}

{#if columns.length > 0}
	<Table>
		<thead>
			<tr>
				<th class="expand-col" aria-hidden="true"></th>
				{#each columns as col, i (col)}
					<th style:width={widths[i] ? `${widths[i]}px` : undefined}>
						<button type="button" class="sort-btn" onclick={() => toggleSort(i)}>
							{col}
							{#if sortCol === i}<span class="sort-ind">{sortDir === 1 ? '▲' : '▼'}</span>{/if}
						</button>
						<span
							class="resize-handle"
							role="separator"
							aria-orientation="vertical"
							aria-label="Resize {col} column"
							onpointerdown={(e) => startResize(e, i, (e.currentTarget.previousElementSibling as HTMLElement)?.offsetWidth ?? 140)}
							onpointermove={onResizeMove}
							onpointerup={onResizeEnd}
						></span>
					</th>
				{/each}
			</tr>
		</thead>
		<tbody>
			{#each sortedRows as row, i (i)}
				<tr
					class="data-row"
					tabindex="0"
					role="button"
					aria-expanded={expanded.has(i)}
					onclick={() => toggleExpanded(i)}
					onkeydown={(e) => {
						if (e.key === 'Enter' || e.key === ' ') {
							e.preventDefault();
							toggleExpanded(i);
						}
					}}
				>
					<td class="expand-col">
						<span class="chevron" class:open={expanded.has(i)} aria-hidden="true">›</span>
					</td>
					{#each row as cell, j (j)}
						<td style:width={widths[j] ? `${widths[j]}px` : undefined}>
							{#if j === severityCol}
								<SeverityBadge severity={formatCell(cell)} />
							{:else}
								<span class="cell-text">{formatCell(cell)}</span>
							{/if}
						</td>
					{/each}
				</tr>
				{#if expanded.has(i)}
					<tr class="detail-row">
						<td colspan={columns.length + 1}>
							<dl>
								{#each columns as col, j (col)}
									<dt>{col}</dt>
									<dd>{formatCell(row[j])}</dd>
								{/each}
							</dl>
						</td>
					</tr>
				{/if}
			{/each}
		</tbody>
	</Table>
{/if}

<style>
	.row-count {
		color: var(--color-text-muted);
		font-size: var(--text-sm);
		margin: var(--space-3) 0 0;
	}
	.expand-col {
		width: 1.5rem;
	}
	.chevron {
		display: inline-block;
		color: var(--color-text-muted);
		transition: transform 0.1s ease;
	}
	.chevron.open {
		transform: rotate(90deg);
	}
	.sort-btn {
		background: none;
		border: none;
		padding: 0;
		font: inherit;
		color: inherit;
		text-transform: inherit;
		letter-spacing: inherit;
		cursor: pointer;
		display: inline-flex;
		align-items: center;
		gap: var(--space-1);
	}
	.sort-ind {
		font-size: 0.6em;
		color: var(--color-accent);
	}
	th {
		position: relative;
	}
	.resize-handle {
		position: absolute;
		right: 0;
		top: 0;
		bottom: 0;
		width: 6px;
		cursor: col-resize;
		touch-action: none;
	}
	.resize-handle:hover {
		background: var(--color-accent);
		opacity: 0.4;
	}
	.data-row {
		cursor: pointer;
	}
	.cell-text {
		overflow: hidden;
		text-overflow: ellipsis;
	}
	.detail-row td {
		background: var(--color-bg);
		padding: var(--space-3) var(--row-padding-x);
	}
	.detail-row dl {
		display: grid;
		grid-template-columns: max-content 1fr;
		gap: var(--space-1) var(--space-4);
		margin: 0;
		font-family: var(--font-mono);
		font-size: var(--text-sm);
	}
	.detail-row dt {
		color: var(--color-text-muted);
	}
	.detail-row dd {
		margin: 0;
		color: var(--color-text);
		word-break: break-word;
	}
</style>
