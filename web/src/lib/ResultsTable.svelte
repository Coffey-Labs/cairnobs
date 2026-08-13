<script lang="ts">
	// Shared between the SQL query page and the free-text search page —
	// both /api endpoints return the same {columns, rows} shape
	// specifically so this component didn't need to exist twice.
	let {
		columns,
		rows,
		hasRun = false
	}: { columns: string[]; rows: unknown[][]; hasRun?: boolean } = $props();

	function formatCell(value: unknown): string {
		if (value === null || value === undefined) return '';
		if (typeof value === 'object') return JSON.stringify(value);
		return String(value);
	}
</script>

{#if hasRun}
	<p>{rows.length} row(s)</p>
{/if}

{#if columns.length > 0}
	<table>
		<thead>
			<tr>
				{#each columns as col (col)}
					<th>{col}</th>
				{/each}
			</tr>
		</thead>
		<tbody>
			{#each rows as row, i (i)}
				<tr>
					{#each row as cell, j (j)}
						<td>{formatCell(cell)}</td>
					{/each}
				</tr>
			{/each}
		</tbody>
	</table>
{/if}

<style>
	table {
		border-collapse: collapse;
		width: 100%;
		margin-top: 1rem;
	}
	th,
	td {
		border: 1px solid #ccc;
		padding: 0.25rem 0.5rem;
		text-align: left;
		font-size: 0.85rem;
	}
	th {
		background: #f0f0f0;
	}
</style>
