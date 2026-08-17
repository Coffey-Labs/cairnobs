<script lang="ts">
	// Extracted from the root query page in Phase 3 so the dashboard panel
	// editor and the alert rule editor can reuse the same input --
	// deliberately just the input+run affordance, not results/history,
	// which differ per consumer. Phase 7 adds AI-assisted authoring
	// (explain/fix/optimize) here rather than as a separate mode/page, so
	// every consumer of this component gets it -- errorMessage/warnings
	// are optional props specifically so existing callers that don't pass
	// them see no behavior change at all.
	import type { Language } from '$lib/api';
	import {
		aiExplain,
		aiFix,
		aiOptimize,
		aiTranslate,
		logInteraction,
		type FixResponse,
		type OptimizeResponse,
		type TranslateResponse
	} from '$lib/api';
	import { Button, Modal } from '$lib/components/ui';
	import QueryEditor from '$lib/query-editor/QueryEditor.svelte';

	let {
		query = $bindable(''),
		language = $bindable<Language>(''),
		onRun,
		loading = false,
		placeholder = 'service=api | where status>=500 | stats count by host | sort -count',
		// Set by the caller after a failed run (parse or execution error)
		// -- presence alone drives the "Fix this" affordance below, this
		// component never calls runQuery itself to find out.
		errorMessage = '',
		// Set by the caller from a successful run's QueryResult.warnings
		// (costguard, Phase 7 task 4) -- presence alone drives the
		// "Optimize" affordance.
		warnings = []
	}: {
		query: string;
		language: Language;
		onRun: () => void;
		loading?: boolean;
		placeholder?: string;
		errorMessage?: string;
		warnings?: string[];
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

	// ---- "looks like natural language" detection (task 10) ----
	//
	// Deliberately NOT "wait for a parse error" -- the pipe grammar's
	// own free-text rule (bare words AND'd together, see
	// /docs/query-language-reference.md's "Free-text search" section)
	// means a plain-English question like "show me errors from the last
	// hour" *parses successfully* as a search for records containing
	// all of those words literally. It never fails to parse; it just
	// silently returns an unhelpful result. Waiting for a parse error
	// would miss the single most common real case this feature exists
	// for. Instead: a cheap client-side heuristic flags text that has
	// none of the pipe syntax's structural markers (`|`, a comparison
	// operator, `:`) and is long enough (4+ words) that it's very
	// unlikely to be an intentional short free-text search someone
	// actually wants run literally -- a single bare word or a quoted
	// phrase is common and legitimate, and stays untouched.
	function looksLikeNaturalLanguage(q: string): boolean {
		const trimmed = q.trim();
		if (trimmed === '' || /^\s*select\b/i.test(trimmed)) return false;
		if (/[|=<>:]/.test(trimmed)) return false;
		return trimmed.split(/\s+/).length >= 4;
	}
	let looksLikeNL = $derived(looksLikeNaturalLanguage(query));

	// ---- interaction audit logging (task 12) ----
	// Fire-and-forget: an audit write failure here must never block or
	// surface an error on the button press that triggered it (same
	// posture as the AI operations themselves being optional). Scoped to
	// translate/fix/optimize only -- see logInteraction's/InteractionLogger's
	// doc comments for why complete and explain are excluded.
	function logFixOrOptimizeInteraction(
		operation: 'fix' | 'optimize',
		input: string,
		suggestedQuery: string,
		accepted: boolean
	) {
		logInteraction({
			operation,
			input,
			output: suggestedQuery,
			accepted,
			edited: false,
			finalQuery: suggestedQuery
		}).catch(() => {});
	}

	// ---- explain ----
	let explainOpen = $state(false);
	let explainLoading = $state(false);
	let explainText = $state('');
	let explainError = $state('');

	async function runExplain() {
		explainOpen = true;
		explainLoading = true;
		explainError = '';
		explainText = '';
		try {
			const res = await aiExplain(query, effectiveLanguage);
			explainText = res.explanation;
		} catch {
			// Any failure (including "AI not configured," a plain 404) is
			// shown as a quiet unavailable message, not a scary error --
			// this is an optional enhancement, not a core feature whose
			// failure should read as something broken.
			explainError = 'AI explanation is not available right now.';
		} finally {
			explainLoading = false;
		}
	}

	// ---- fix ----
	let fixOpen = $state(false);
	let fixLoading = $state(false);
	let fixResult: FixResponse | null = $state(null);
	let fixError = $state('');

	async function runFix() {
		fixOpen = true;
		fixLoading = true;
		fixError = '';
		fixResult = null;
		try {
			fixResult = await aiFix(query, effectiveLanguage, { executionError: errorMessage });
		} catch {
			fixError = 'AI fix suggestions are not available right now.';
		} finally {
			fixLoading = false;
		}
	}

	function acceptFix() {
		if (!fixResult?.suggestedQuery) return;
		logFixOrOptimizeInteraction('fix', query, fixResult.suggestedQuery, true);
		query = fixResult.suggestedQuery;
		fixOpen = false;
	}

	function dismissFix() {
		if (fixResult?.suggestedQuery) {
			logFixOrOptimizeInteraction('fix', query, fixResult.suggestedQuery, false);
		}
		fixOpen = false;
	}

	// ---- optimize ----
	let optimizeOpen = $state(false);
	let optimizeLoading = $state(false);
	let optimizeResult: OptimizeResponse | null = $state(null);
	let optimizeError = $state('');

	async function runOptimize() {
		optimizeOpen = true;
		optimizeLoading = true;
		optimizeError = '';
		optimizeResult = null;
		try {
			optimizeResult = await aiOptimize(query, effectiveLanguage);
		} catch {
			optimizeError = 'AI optimization suggestions are not available right now.';
		} finally {
			optimizeLoading = false;
		}
	}

	function acceptOptimize() {
		if (!optimizeResult?.suggestedQuery) return;
		logFixOrOptimizeInteraction('optimize', query, optimizeResult.suggestedQuery, true);
		query = optimizeResult.suggestedQuery;
		optimizeOpen = false;
	}

	function dismissOptimize() {
		if (optimizeResult?.suggestedQuery) {
			logFixOrOptimizeInteraction('optimize', query, optimizeResult.suggestedQuery, false);
		}
		optimizeOpen = false;
	}

	// ---- translate (Track B) ----
	let translateOpen = $state(false);
	let translateLoading = $state(false);
	let translateNL = $state('');
	let translateResult: TranslateResponse | null = $state(null);
	let translateExplanation = $state('');
	let translateError = $state('');
	// Tracks edits made in the review textarea after a result arrives --
	// see editedQuery's own comment below for why this exists.
	let editedQuery = $state('');

	function openTranslate() {
		// Pre-fill with the current query bar content -- that's exactly
		// what triggered the "looks like natural language" affordance in
		// the first place, so re-typing it would be pure friction.
		translateNL = query;
		translateOpen = true;
		runTranslate();
	}

	async function runTranslate() {
		translateLoading = true;
		translateError = '';
		translateResult = null;
		translateExplanation = '';
		try {
			const res = await aiTranslate(translateNL);
			translateResult = res;
			editedQuery = res.query;
			if (res.query && res.compiles) {
				// Reuses Explain rather than building a separate
				// "describe the translation" mechanism -- task 10's
				// explicit instruction. OriginalIntent lets the prompt
				// speak to *how* the question became this query, not
				// just describe the query in isolation.
				try {
					const explainRes = await aiExplain(res.query, 'spl', translateNL);
					translateExplanation = explainRes.explanation;
				} catch {
					// Explanation is a nice-to-have on top of the
					// translation itself -- a failure here shouldn't
					// blank out an otherwise-successful translation.
				}
			}
		} catch {
			translateError = 'AI translation is not available right now.';
		} finally {
			translateLoading = false;
		}
	}

	function useTranslatedQuery() {
		if (!editedQuery.trim()) return;
		if (translateResult) {
			logInteraction({
				operation: 'translate',
				input: translateNL,
				output: translateResult.query,
				confidence: translateResult.confidence,
				accepted: true,
				edited: editedQuery !== translateResult.query,
				finalQuery: editedQuery
			}).catch(() => {});
		}
		query = editedQuery;
		translateOpen = false;
	}

	function cancelTranslate() {
		if (translateResult?.query) {
			logInteraction({
				operation: 'translate',
				input: translateNL,
				output: translateResult.query,
				confidence: translateResult.confidence,
				accepted: false,
				edited: false,
				finalQuery: translateResult.query
			}).catch(() => {});
		}
		translateOpen = false;
	}

	// A blocked suggestion the user has since edited is their own text
	// now, not the original flagged one -- costguard will assess
	// whatever they actually run anyway (every /query response carries
	// its own warnings, Track A task 4), so re-blocking an edit they
	// made specifically to address the concern would be actively
	// unhelpful, not extra-safe.
	let translateUseDisabled = $derived.by(() => {
		if (!editedQuery.trim()) return true;
		const result = translateResult;
		if (!result) return false;
		return result.blocked && editedQuery === result.query;
	});
</script>

<div class="query-bar">
	<QueryEditor bind:value={query} {onRun} {placeholder} language={effectiveLanguage} />
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
		<Button variant="ghost" size="sm" onclick={runExplain} disabled={query.trim() === ''}>
			Explain this query
		</Button>
		{#if errorMessage}
			<Button variant="ghost" size="sm" onclick={runFix}>Try AI fix</Button>
		{/if}
		{#if warnings.length > 0}
			<Button variant="ghost" size="sm" onclick={runOptimize}>Optimize</Button>
		{/if}
		{#if looksLikeNL}
			<Button variant="ghost" size="sm" onclick={openTranslate}>Interpret as natural language</Button>
		{/if}
		<span class="hint">⌘/Ctrl+Enter to run</span>
	</div>
	{#if warnings.length > 0}
		<p class="cost-warning">⚠ {warnings.join('; ')}</p>
	{/if}
</div>

<Modal bind:open={explainOpen} title="What this query does">
	{#if explainLoading}
		<p class="muted">Thinking…</p>
	{:else if explainError}
		<p class="muted">{explainError}</p>
	{:else}
		<p>{explainText}</p>
	{/if}
</Modal>

<Modal bind:open={fixOpen} title="AI-suggested fix">
	{#if fixLoading}
		<p class="muted">Thinking…</p>
	{:else if fixError}
		<p class="muted">{fixError}</p>
	{:else if fixResult}
		{#if !fixResult.suggestedQuery}
			<p class="muted">
				{fixResult.explanation || "The AI couldn't determine a fix for this error."}
			</p>
		{:else}
			<div class="diff">
				<div class="diff-row removed">
					<span class="diff-label">Current</span>
					<code>{query}</code>
				</div>
				<div class="diff-row added">
					<span class="diff-label">Suggested</span>
					<code>{fixResult.suggestedQuery}</code>
				</div>
			</div>
			{#if fixResult.explanation}<p>{fixResult.explanation}</p>{/if}
			{#if fixResult.blocked}
				<p class="cost-warning">
					⚠ This suggestion isn't offered as directly runnable: {(fixResult.costWarnings ?? []).join('; ')}
					You can still copy it and adjust manually.
				</p>
			{/if}
			<div class="actions">
				<Button variant="secondary" onclick={dismissFix}>Dismiss</Button>
				<Button variant="primary" onclick={acceptFix} disabled={fixResult.blocked}>
					Accept
				</Button>
			</div>
		{/if}
	{/if}
</Modal>

<Modal bind:open={optimizeOpen} title="Optimize this query">
	{#if optimizeLoading}
		<p class="muted">Thinking…</p>
	{:else if optimizeError}
		<p class="muted">{optimizeError}</p>
	{:else if optimizeResult}
		<p>{optimizeResult.phrased || optimizeResult.findings.join('; ')}</p>
		{#if optimizeResult.suggestedQuery}
			<div class="diff">
				<div class="diff-row removed">
					<span class="diff-label">Current</span>
					<code>{query}</code>
				</div>
				<div class="diff-row added">
					<span class="diff-label">Suggested</span>
					<code>{optimizeResult.suggestedQuery}</code>
				</div>
			</div>
			<div class="actions">
				<Button variant="secondary" onclick={dismissOptimize}>Dismiss</Button>
				<Button variant="primary" onclick={acceptOptimize}>Accept</Button>
			</div>
		{/if}
	{/if}
</Modal>

<Modal bind:open={translateOpen} title="Ask in plain English">
	<label class="nl-label" for="nl-question">Your question</label>
	<textarea id="nl-question" class="nl-input" bind:value={translateNL} rows="2"></textarea>
	<div class="actions translate-actions">
		<Button variant="secondary" size="sm" onclick={runTranslate} disabled={translateLoading || !translateNL.trim()}>
			{translateLoading ? 'Translating…' : 'Translate again'}
		</Button>
	</div>

	{#if translateLoading && !translateResult}
		<p class="muted">Thinking…</p>
	{:else if translateError}
		<p class="muted">{translateError}</p>
	{:else if translateResult}
		{#if !translateResult.query}
			<!-- Honest low-confidence handling (task 10): no guess shown
			     with false confidence, just the reason plainly stated. -->
			<p class="muted">
				{translateResult.lowConfidenceReason || "Not confident enough to guess -- try rephrasing."}
			</p>
		{:else}
			{#if translateResult.confidence === 'low'}
				<p class="cost-warning">
					⚠ Low confidence{translateResult.lowConfidenceReason ? `: ${translateResult.lowConfidenceReason}` : ''}. Review carefully before using.
				</p>
			{/if}
			<label class="nl-label" for="nl-generated">Generated query (editable)</label>
			<textarea id="nl-generated" class="nl-input mono" bind:value={editedQuery} rows="3"></textarea>
			{#if !translateResult.compiles}
				<p class="cost-warning">⚠ This doesn't parse as a valid query: {translateResult.compileError}. Edit it above before using.</p>
			{/if}
			{#if translateExplanation}<p>{translateExplanation}</p>{/if}
			{#if translateResult.blocked && editedQuery === translateResult.query}
				<p class="cost-warning">
					⚠ Not offered as directly runnable: {(translateResult.costWarnings ?? []).join('; ')} Edit the query above to address this, or copy it manually.
				</p>
			{/if}
			<div class="actions">
				<Button variant="secondary" onclick={cancelTranslate}>Cancel</Button>
				<Button variant="primary" onclick={useTranslatedQuery} disabled={translateUseDisabled}>
					Use this query
				</Button>
			</div>
		{/if}
	{/if}
</Modal>

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
	.cost-warning {
		margin-top: var(--space-2);
		font-size: var(--text-sm);
		color: var(--color-sev-warn);
	}
	.muted {
		color: var(--color-text-muted);
	}
	.diff {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		font-family: var(--font-mono);
		font-size: var(--text-sm);
		margin-bottom: var(--space-3);
	}
	.diff-row {
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
		padding: var(--space-2);
		border-radius: var(--radius-sm);
	}
	.diff-row code {
		white-space: pre-wrap;
		word-break: break-word;
	}
	.diff-row.removed {
		background: var(--color-sev-error-bg);
	}
	.diff-row.added {
		background: var(--color-sev-info-bg);
	}
	.diff-label {
		font-family: var(--font-ui);
		font-size: var(--text-xs);
		color: var(--color-text-muted);
	}
	.actions {
		display: flex;
		justify-content: flex-end;
		gap: var(--space-2);
	}
	.nl-label {
		display: block;
		font-size: var(--text-xs);
		color: var(--color-text-muted);
		margin-bottom: var(--space-1);
	}
	.nl-input {
		width: 100%;
		box-sizing: border-box;
		background: var(--color-surface);
		color: var(--color-text);
		border: 1px solid var(--color-border);
		border-radius: var(--radius-sm);
		padding: var(--space-2);
		font-family: var(--font-ui);
		font-size: var(--text-sm);
		resize: vertical;
	}
	.nl-input.mono {
		font-family: var(--font-mono);
	}
	.translate-actions {
		justify-content: flex-start;
		margin: var(--space-2) 0 var(--space-3);
	}
</style>
