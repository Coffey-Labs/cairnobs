<script lang="ts">
	// Phase 2: single unified query page. Replaces Phase 0/1's two
	// separate pages (raw-SQL-only /query, free-text-only /search) --
	// see /docs/query-language-design.md and /docs/query-language-reference.md.
	// Phase 3: query bar extracted into $lib/QueryBar.svelte (reused by
	// the dashboard panel editor and alert rule editor), fetch calls
	// moved into $lib/api.ts.

	import ResultsTable from '$lib/ResultsTable.svelte';
	import QueryBar from '$lib/QueryBar.svelte';
	import AddToDashboardModal from '$lib/components/AddToDashboardModal.svelte';
	import { Button } from '$lib/components/ui';
	import { runQuery as apiRunQuery, injectTimeRange, type Language } from '$lib/api';
	import { page } from '$app/state';

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

	// Drill-down landing: a chart's "click to drill into query"
	// (see $lib/charts/drilldown.ts) navigates here with ?q=&earliest=&latest=.
	// Runs once per navigation, not on every reactive change, so editing
	// the query bar afterwards doesn't keep re-injecting the original
	// drill-down range.
	let consumedDrillDownParams = false;
	$effect(() => {
		const params = page.url.searchParams;
		const q = params.get('q');
		if (!q || consumedDrillDownParams) return;
		consumedDrillDownParams = true;
		const earliest = params.get('earliest');
		const latest = params.get('latest');
		query = earliest ? injectTimeRange(q, earliest, latest ?? 'now') : q;
		runQuery();
	});

	let addToDashboardOpen = $state(false);
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

	{#if hasRun && !error && columns.length > 0}
		<div class="results-actions">
			<Button size="sm" variant="secondary" onclick={() => (addToDashboardOpen = true)}>
				+ Add as panel to dashboard
			</Button>
		</div>
	{/if}

	<ResultsTable {columns} {rows} {hasRun} />

	<AddToDashboardModal bind:open={addToDashboardOpen} {query} {language} />

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
		max-width: 64rem;
	}
	h1 {
		font-size: var(--text-xl);
		margin-bottom: var(--space-2);
	}
	p {
		color: var(--color-text-muted);
	}
	.error {
		color: var(--color-danger);
	}
	.results-actions {
		margin: var(--space-3) 0;
	}
	.history ul {
		list-style: none;
		padding: 0;
		margin: var(--space-2) 0 0;
	}
	.history-item {
		background: none;
		border: none;
		text-align: left;
		padding: var(--space-1) 0;
		cursor: pointer;
		color: var(--color-accent);
		font-family: var(--font-mono);
	}
	.history-item:hover {
		text-decoration: underline;
	}
	.cheatsheet {
		margin-top: var(--space-5);
		font-size: var(--text-sm);
		color: var(--color-text-muted);
	}
	.cheatsheet table {
		border-collapse: collapse;
		margin-top: var(--space-2);
		font-family: var(--font-mono);
	}
	.cheatsheet td {
		padding: var(--space-1) var(--space-4) var(--space-1) 0;
		vertical-align: top;
	}
</style>
