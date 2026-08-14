<script lang="ts">
	// Phase 2: single unified query page. Replaces Phase 0/1's two
	// separate pages (raw-SQL-only /query, free-text-only /search) --
	// see /docs/query-language-design.md and /docs/query-language-reference.md.
	// Phase 3: query bar extracted into $lib/QueryBar.svelte (reused by
	// the dashboard panel editor and alert rule editor), fetch calls
	// moved into $lib/api.ts.

	import ResultsTable from '$lib/ResultsTable.svelte';
	import QueryBar from '$lib/QueryBar.svelte';
	import { runQuery as apiRunQuery, type Language } from '$lib/api';

	type HistoryEntry = { query: string; language: Language; at: number };

	const HISTORY_KEY = 'sentry.queryHistory';
	const HISTORY_LIMIT = 20;

	let query = $state('earliest=-1h | sort -timestamp | head 100');
	let language = $state<Language>('');
	let columns = $state<string[]>([]);
	let rows = $state<unknown[][]>([]);
	let error = $state('');
	let loading = $state(false);
	let hasRun = $state(false);
	let history = $state<HistoryEntry[]>(loadHistory());

	function loadHistory(): HistoryEntry[] {
		if (typeof sessionStorage === 'undefined') return [];
		try {
			const raw = sessionStorage.getItem(HISTORY_KEY);
			return raw ? JSON.parse(raw) : [];
		} catch {
			return [];
		}
	}

	function saveHistory(entry: HistoryEntry) {
		history = [entry, ...history.filter((h) => h.query !== entry.query)].slice(0, HISTORY_LIMIT);
		try {
			sessionStorage.setItem(HISTORY_KEY, JSON.stringify(history));
		} catch {
			// session storage unavailable/full -- history is a convenience,
			// not worth failing the query over
		}
	}

	function useHistoryEntry(entry: HistoryEntry) {
		query = entry.query;
		language = entry.language;
	}

	async function runQuery() {
		loading = true;
		error = '';
		try {
			const result = await apiRunQuery(query, language);
			columns = result.columns ?? [];
			rows = result.rows ?? [];
			saveHistory({ query, language, at: Date.now() });
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
	<h1>Sentry — Query</h1>
	<p>
		One query bar for both filter/stats queries and free-text search — see
		<code>/docs/query-language-reference.md</code> in the repo for the full syntax, or the cheat
		sheet below. Build reusable queries into a <a href="/dashboards">dashboard</a>.
	</p>

	<QueryBar bind:query bind:language onRun={runQuery} {loading} />

	{#if error}
		<p class="error">Error: {error}</p>
	{/if}

	<ResultsTable {columns} {rows} {hasRun} />

	{#if history.length > 0}
		<details class="history">
			<summary>Query history ({history.length})</summary>
			<ul>
				{#each history as entry (entry.at)}
					<li>
						<button class="history-item" onclick={() => useHistoryEntry(entry)}>
							<code>{entry.query}</code>
						</button>
					</li>
				{/each}
			</ul>
		</details>
	{/if}

	<details class="cheatsheet">
		<summary>Pipe syntax cheat sheet</summary>
		<table>
			<tbody>
				<tr><td><code>field=value</code></td><td>filter on a structured field</td></tr>
				<tr><td><code>"free text"</code> / bare word</td><td>full-text search on <code>message</code></td></tr>
				<tr><td><code>message:"exact phrase"</code></td><td>explicit full-text search</td></tr>
				<tr><td><code>| where field&gt;value</code></td><td>additional structured filter</td></tr>
				<tr><td><code>| stats count by field</code></td><td>aggregate (count/sum/avg/min/max)</td></tr>
				<tr><td><code>| sort -field</code></td><td>sort descending (<code>+field</code> for ascending)</td></tr>
				<tr><td><code>| fields a, b</code></td><td>project specific columns</td></tr>
				<tr><td><code>| head 50</code> / <code>| tail 50</code></td><td>limit results</td></tr>
				<tr><td><code>earliest=-1h</code> / <code>latest=...</code></td><td>time range (relative or RFC3339)</td></tr>
			</tbody>
		</table>
		<p>Full reference: <code>/docs/query-language-reference.md</code> in the repo.</p>
	</details>
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
	.history ul {
		list-style: none;
		padding: 0;
		margin: 0.5rem 0 0;
	}
	.history-item {
		background: none;
		border: none;
		text-align: left;
		padding: 0.25rem 0;
		cursor: pointer;
		color: #06c;
	}
	.history-item:hover {
		text-decoration: underline;
	}
	.cheatsheet {
		margin-top: 1.5rem;
		font-size: 0.85rem;
	}
	.cheatsheet table {
		border-collapse: collapse;
		margin-top: 0.5rem;
	}
	.cheatsheet td {
		padding: 0.2rem 0.75rem 0.2rem 0;
		vertical-align: top;
	}
</style>
