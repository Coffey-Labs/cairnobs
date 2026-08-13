<script lang="ts">
	// Phase 1: free-text search via POST /search on the api service, hits
	// the Tantivy-backed search service and returns full rows joined back
	// against ClickHouse. No unified query experience with the SQL page —
	// that's Phase 2's job.

	import ResultsTable from '$lib/ResultsTable.svelte';

	const apiBase = import.meta.env.VITE_API_BASE_URL ?? 'http://localhost:8080';

	let query = $state('');
	let columns = $state<string[]>([]);
	let rows = $state<unknown[][]>([]);
	let error = $state('');
	let loading = $state(false);
	let hasRun = $state(false);

	async function runSearch() {
		loading = true;
		error = '';
		try {
			const res = await fetch(`${apiBase}/search`, {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ query })
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
	<h1>Sentry — Full-Text Search</h1>
	<p>
		Free-text search over the <code>message</code> field, via Tantivy. Supports plain terms,
		<code>"exact phrases"</code>, and <code>wildcard*</code> — see
		<code>/search</code> for the full query syntax. Looking for structured/aggregation queries
		instead? See the <a href="/">SQL Query</a> page.
	</p>

	<input
		type="text"
		bind:value={query}
		placeholder="e.g. &quot;connection refused&quot; or timeout*"
		spellcheck="false"
		onkeydown={(e) => e.key === 'Enter' && runSearch()}
	/>
	<div>
		<button onclick={runSearch} disabled={loading || query.trim() === ''}>
			{loading ? 'Searching…' : 'Search'}
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
	input {
		width: 100%;
		font-family: monospace;
		font-size: 0.9rem;
		padding: 0.4rem;
		box-sizing: border-box;
	}
	button {
		margin-top: 0.5rem;
	}
	.error {
		color: #b00020;
	}
</style>
