<script lang="ts">
	// Extracted from the root query page in Phase 3 so the dashboard panel
	// editor and the alert rule editor can reuse the same input --
	// deliberately just the input+run affordance, not results/history,
	// which differ per consumer.
	import type { Language } from '$lib/api';

	let {
		query = $bindable(''),
		language = $bindable<Language>(''),
		onRun,
		loading = false,
		placeholder = 'service=api | where status>=500 | stats count by host | sort -count'
	}: {
		query: string;
		language: Language;
		onRun: () => void;
		loading?: boolean;
		placeholder?: string;
	} = $props();

	// Client-side mirror of the backend's auto-detect heuristic
	// (api/internal/querylang/planner.looksLikeSQL) -- purely a UI hint,
	// the server does its own detection independently and is the
	// authority on what actually runs.
	function detectedLanguage(q: string): 'sql' | 'spl' {
		return /^\s*select\b/i.test(q) ? 'sql' : 'spl';
	}
	let detected = $derived(detectedLanguage(query));
	let effectiveLanguage = $derived(language === '' ? detected : language);

	function onKeydown(e: KeyboardEvent) {
		// Cmd/Ctrl+Enter runs the query -- textarea's own Enter key needs
		// to stay newline-for-pipe-stage-formatting, so this isn't a bare
		// Enter binding.
		if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') {
			e.preventDefault();
			onRun();
		}
	}
</script>

<div class="query-bar">
	<textarea bind:value={query} onkeydown={onKeydown} rows="4" spellcheck="false" {placeholder}></textarea>
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
		<button onclick={onRun} disabled={loading || query.trim() === ''}>
			{loading ? 'Running…' : 'Run query'}
		</button>
		<span class="hint">⌘/Ctrl+Enter to run</span>
	</div>
</div>

<style>
	.query-bar textarea {
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
</style>
