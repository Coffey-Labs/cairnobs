<script lang="ts">
	// CodeMirror wrapper -- replaces QueryBar's plain <textarea>. Chosen
	// over hand-rolling syntax highlighting (the classic "transparent
	// textarea over a colored <pre>" overlay trick) because autocomplete
	// needs real cursor/viewport-aware popup positioning and keyboard
	// handling that trick doesn't give you for free, and CodeMirror is a
	// well-established dependency for exactly this job (the same
	// "reach for a real editor primitive" reasoning ECharts and GridStack
	// already followed elsewhere in Phase 5).
	import { EditorView, keymap, placeholder as placeholderExt } from '@codemirror/view';
	import { autocompletion, closeBrackets, completionKeymap } from '@codemirror/autocomplete';
	import { defaultKeymap, history, historyKeymap } from '@codemirror/commands';
	import { pipeLanguage, pipeSyntaxHighlighting } from './language';
	import { pipeCompletions } from './completions';

	let {
		value = $bindable(''),
		onRun,
		placeholder = '',
		ariaLabel = 'Query'
	}: { value?: string; onRun?: () => void; placeholder?: string; ariaLabel?: string } = $props();

	let container: HTMLDivElement | undefined = $state();
	let view: EditorView | undefined;
	// Tracks the last value *this component* pushed into `value`, so the
	// external-sync effect below can tell "value changed because someone
	// else set the bindable prop" (e.g. clicking a history entry) apart
	// from "value changed because the updateListener below just set it
	// from the user's own typing" -- without this, every keystroke would
	// round-trip through both effects, and on fast/multi-character input
	// (e.g. automation tools that insert text in one burst) the second
	// effect could dispatch a stale sync in between keystrokes and drop
	// characters.
	let lastEmitted = '';

	const theme = EditorView.theme({
		'&': {
			fontFamily: 'var(--font-mono)',
			fontSize: 'var(--text-base)',
			backgroundColor: 'var(--color-surface)',
			border: '1px solid var(--color-border)',
			borderRadius: 'var(--radius-sm)'
		},
		'&.cm-focused': {
			outline: 'none',
			borderColor: 'var(--color-accent)'
		},
		'.cm-content': { padding: 'var(--space-3)', color: 'var(--color-text)', caretColor: 'var(--color-accent)' },
		'.cm-cursor': { borderLeftColor: 'var(--color-accent)' },
		'.cm-scroller': { minHeight: '4.5rem' },
		'.cm-placeholder': { color: 'var(--color-text-faint)' },
		'.cm-tooltip-autocomplete': {
			backgroundColor: 'var(--color-surface-raised)',
			border: '1px solid var(--color-border)',
			borderRadius: 'var(--radius-sm)',
			fontFamily: 'var(--font-mono)',
			fontSize: 'var(--text-sm)'
		},
		'.cm-tooltip-autocomplete ul li[aria-selected]': {
			backgroundColor: 'var(--color-accent)',
			color: 'var(--color-on-accent)'
		},
		'.cm-selectionBackground': { backgroundColor: 'color-mix(in srgb, var(--color-accent) 25%, transparent) !important' }
	});

	$effect(() => {
		if (!container) return;
		lastEmitted = value;
		view = new EditorView({
			doc: value,
			parent: container,
			extensions: [
				pipeLanguage,
				pipeSyntaxHighlighting,
				theme,
				history(),
				closeBrackets(),
				autocompletion({ override: [pipeCompletions] }),
				placeholderExt(placeholder),
				EditorView.contentAttributes.of({ 'aria-label': ariaLabel }),
				keymap.of([
					...completionKeymap,
					...defaultKeymap,
					...historyKeymap,
					{
						key: 'Mod-Enter',
						run: () => {
							onRun?.();
							return true;
						}
					}
				]),
				EditorView.lineWrapping,
				EditorView.updateListener.of((update) => {
					if (update.docChanged) {
						lastEmitted = update.state.doc.toString();
						value = lastEmitted;
					}
				})
			]
		});
		return () => view?.destroy();
	});

	// External changes to `value` (e.g. clicking a history entry) need to
	// push into the editor -- the updateListener above only covers the
	// other direction (editor -> value). Compares against `lastEmitted`,
	// not the view's live doc: comparing against the view's doc directly
	// re-fires on every one of *this component's own* edits too (each
	// keystroke changes `value`, which re-runs this effect), and on fast
	// or multi-character input that redundant dispatch can land between
	// keystrokes and clobber characters the updateListener just applied.
	$effect(() => {
		if (!view) return;
		if (value !== lastEmitted) {
			lastEmitted = value;
			view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: value } });
		}
	});
</script>

<div bind:this={container} class="query-editor"></div>

<style>
	.query-editor {
		width: 100%;
	}
</style>
