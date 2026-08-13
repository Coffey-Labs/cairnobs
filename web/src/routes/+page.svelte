<script lang="ts">
	// Phase 0: functional only, no styling polish, no auth. One page: a raw
	// SQL box against POST /query on the api service, rendered as a table.
	// This is a placeholder for the real query UI that lands once /api grows
	// a real SPL-like query layer in Phase 2.

	import ResultsTable from '$lib/ResultsTable.svelte';

	const apiBase = import.meta.env.VITE_API_BASE_URL ?? 'http://localhost:8080';

	let sql = $state('SELECT * FROM logs ORDER BY timestamp DESC LIMIT 100');
	let columns = $state<string[]>([]);
	let rows = $state<unknown[][]>([]);
	let error = $state('');
	let loading = $state(false);
	let hasRun = $state(false);

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
	<h1>Sentry — Log Query</h1>
	<p>
		Raw SQL only, SELECT statements against the <code>logs</code> table. No auth, no query
		builder yet — see <code>/api</code> for what's actually allowed. Looking for free-text
		search instead? See the <a href="/search">Full-Text Search</a> page.
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

	<ResultsTable {columns} {rows} {hasRun} />
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
</style>
