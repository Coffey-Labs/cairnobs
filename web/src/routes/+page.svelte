<script lang="ts">
	// Phase 0: functional only, no styling polish, no auth. One page: a raw
	// SQL box against POST /query on the api service, rendered as a table.
	// This is a placeholder for the real query UI that lands once /api grows
	// a real SPL-like query layer in Phase 2.

	const apiBase = import.meta.env.VITE_API_BASE_URL ?? 'http://localhost:8080';

	let sql = $state('SELECT * FROM logs ORDER BY timestamp DESC LIMIT 100');
	let columns = $state<string[]>([]);
	let rows = $state<unknown[][]>([]);
	let error = $state('');
	let loading = $state(false);
	let hasRun = $state(false);

	function formatCell(value: unknown): string {
		if (value === null || value === undefined) return '';
		if (typeof value === 'object') return JSON.stringify(value);
		return String(value);
	}

	async function runQuery() {
		loading = true;
		error = '';
		try {
			const res = await fetch(`${apiBase}/query`, {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ sql })
			});
			const body = await res.json();
			if (!res.ok) {
				error = body?.error ?? `request failed with status ${res.status}`;
				columns = [];
				rows = [];
				return;
			}
			columns = body.columns ?? [];
			rows = body.rows ?? [];
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
			columns = [];
			rows = [];
		} finally {
			loading = false;
			hasRun = true;
		}
	}
</script>

<main>
	<h1>Sentry — Log Query (Phase 0)</h1>
	<p>
		Raw SQL only, SELECT statements against the <code>logs</code> table. No auth, no query
		builder yet — see <code>/api</code> for what's actually allowed.
	</p>

	<textarea bind:value={sql} rows="4" cols="100" spellcheck="false"></textarea>
	<div>
		<button onclick={runQuery} disabled={loading}>
			{loading ? 'Running…' : 'Run query'}
		</button>
	</div>

	{#if error}
		<p class="error">Error: {error}</p>
	{/if}

	{#if hasRun && !error}
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
</main>

<style>
	main {
		font-family: system-ui, sans-serif;
		max-width: 960px;
		margin: 2rem auto;
		padding: 0 1rem;
	}
	textarea {
		width: 100%;
		font-family: monospace;
		font-size: 0.9rem;
	}
	button {
		margin-top: 0.5rem;
	}
	.error {
		color: #b00020;
	}
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
