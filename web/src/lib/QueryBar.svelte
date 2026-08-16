<script lang="ts">
	// Extracted from the root query page in Phase 3 so the dashboard panel
	// editor and the alert rule editor can reuse the same input --
	// deliberately just the input+run affordance, not results/history,
	// which differ per consumer.
	import type { Language } from '$lib/api';
	import { Button } from '$lib/components/ui';
	import QueryEditor from '$lib/query-editor/QueryEditor.svelte';

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
</script>

<div class="query-bar">
	<QueryEditor bind:value={query} {onRun} {placeholder} />
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
		<Button variant="primary" onclick={onRun} disabled={loading || query.trim() === ''}>
			{loading ? 'Running…' : 'Run query'}
		</Button>
		<span class="hint">⌘/Ctrl+Enter to run</span>
	</div>
</div>

<style>
	.controls {
		margin-top: var(--space-3);
		display: flex;
		align-items: center;
		gap: var(--space-3);
		flex-wrap: wrap;
	}
	.controls select {
		background: var(--color-surface);
		color: var(--color-text);
		border: 1px solid var(--color-border);
		border-radius: var(--radius-sm);
		height: var(--control-height);
		font-family: var(--font-ui);
	}
	.detected-badge {
		font-size: var(--text-xs);
		padding: 0.15rem var(--space-2);
		border-radius: var(--radius-full);
		background: var(--color-sev-info-bg);
		color: var(--color-sev-info);
	}
	.detected-badge.sql {
		background: var(--color-sev-warn-bg);
		color: var(--color-sev-warn);
	}
	.hint {
		font-size: var(--text-sm);
		color: var(--color-text-muted);
	}
</style>
