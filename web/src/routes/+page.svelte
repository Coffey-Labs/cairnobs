<script lang="ts">
	// Phase 2: single unified query page. Replaces Phase 0/1's two
	// separate pages (raw-SQL-only /query, free-text-only /search) --
	// see /docs/query-language-design.md and /docs/query-language-reference.md.

	import ResultsTable from '$lib/ResultsTable.svelte';

	const apiBase = import.meta.env.VITE_API_BASE_URL ?? 'http://localhost:8080';

	type Language = '' | 'sql' | 'spl';
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

	// Client-side mirror of the backend's auto-detect heuristic
	// (api/internal/querylang/planner.looksLikeSQL) -- purely a UI hint,
	// the server does its own detection independently and is the
	// authority on what actually runs.
	function detectedLanguage(q: string): 'sql' | 'spl' {
		return /^\s*select\b/i.test(q) ? 'sql' : 'spl';
	}
	let detected = $derived(detectedLanguage(query));
	let effectiveLanguage = $derived(language === '' ? detected : language);

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
			const res = await fetch(`${apiBase}/query`, {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ query, language })
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

	function onKeydown(e: KeyboardEvent) {
		// Cmd/Ctrl+Enter runs the query -- textarea's own Enter key needs
		// to stay newline-for-pipe-stage-formatting, so this isn't a bare
		// Enter binding.
		if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') {
			e.preventDefault();
			runQuery();
		}
	}
</script>

<main>
	<h1>Sentry — Query</h1>
	<p>
		One query bar for both filter/stats queries and free-text search — see
		<code>/docs/query-language-reference.md</code> in the repo for the full syntax, or the cheat
		sheet below.
	</p>

	<textarea
		bind:value={query}
		onkeydown={onKeydown}
		rows="4"
		cols="100"
		spellcheck="false"
		placeholder={'service=api | where status>=500 | stats count by host | sort -count'}
	></textarea>

	<div class="controls">
		<label>
			Language:
			<select bind:value={language}>
				<option value="">Auto ({detected})</option>
				<option value="spl">Pipe syntax</option>
				<option value="sql">SQL</option>
			</select>
		</label>
		<span class="detected-badge" class:sql={effectiveLanguage === 'sql'}>
			{effectiveLanguage === 'sql' ? 'SQL' : 'pipe syntax'}
		</span>
		<button onclick={runQuery} disabled={loading || query.trim() === ''}>
			{loading ? 'Running…' : 'Run query'}
		</button>
		<span class="hint">⌘/Ctrl+Enter to run</span>
	</div>

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
	textarea {
		width: 100%;
		font-family: monospace;
		font-size: 0.9rem;
		box-sizing: border-box;
	}
	.controls {
		margin-top: 0.5rem;
		display: flex;
		align-items: center;
		gap: 0.75rem;
		flex-wrap: wrap;
	}
	.detected-badge {
		font-size: 0.75rem;
		padding: 0.15rem 0.5rem;
		border-radius: 1rem;
		background: #eef;
		color: #224;
	}
	.detected-badge.sql {
		background: #fee;
		color: #422;
	}
	.hint {
		font-size: 0.8rem;
		color: #777;
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
