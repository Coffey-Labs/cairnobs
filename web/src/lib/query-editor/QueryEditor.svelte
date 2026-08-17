<script lang="ts">
	// CodeMirror wrapper -- replaces QueryBar's plain <textarea>. Chosen
	// over hand-rolling syntax highlighting (the classic "transparent
	// textarea over a colored <pre>" overlay trick) because autocomplete
	// needs real cursor/viewport-aware popup positioning and keyboard
	// handling that trick doesn't give you for free, and CodeMirror is a
	// well-established dependency for exactly this job (the same
	// "reach for a real editor primitive" reasoning ECharts and GridStack
	// already followed elsewhere in Phase 5).
	import { untrack } from 'svelte';
	import {
		EditorView,
		keymap,
		placeholder as placeholderExt,
		Decoration,
		WidgetType,
		type DecorationSet
	} from '@codemirror/view';
	import { StateField, StateEffect } from '@codemirror/state';
	import { autocompletion, closeBrackets, completionKeymap } from '@codemirror/autocomplete';
	import { defaultKeymap, history, historyKeymap } from '@codemirror/commands';
	import { pipeLanguage, pipeSyntaxHighlighting } from './language';
	import { pipeCompletions } from './completions';
	import { aiComplete } from '$lib/api';

	let {
		value = $bindable(''),
		onRun,
		placeholder = '',
		ariaLabel = 'Query',
		language = 'spl',
		aiCompleteEnabled = true
	}: {
		value?: string;
		onRun?: () => void;
		placeholder?: string;
		ariaLabel?: string;
		// language, not the bindable Language type QueryBar owns -- this
		// component only needs the string to pass through to /ai/complete,
		// never interprets it itself.
		language?: string;
		// Baseline autocomplete (pipeCompletions, above) is always on --
		// this only gates the AI ghost-text layer, so a caller embedding
		// QueryEditor somewhere AI assistance doesn't make sense (if any)
		// can opt out without losing deterministic completion. Task 5's
		// "AI assistance augments, never replaces" holds either way: the
		// two mechanisms are fully independent extensions, not one
		// swapped for the other.
		aiCompleteEnabled?: boolean;
	} = $props();

	// ---- ghost-text AI completion (task 5) ----
	// A StateField + a Decoration.widget, not @codemirror/autocomplete's
	// dropdown machinery -- ghost text renders inline after the cursor
	// and accepts on Tab, a genuinely different interaction from a
	// completion list, so it gets its own small extension rather than
	// contorting autocompletion() into rendering it. Hand-built on
	// CodeMirror's own primitives (already a Phase 5 dependency) rather
	// than pulling in a new inline-completion package -- this codebase's
	// "boring, well-understood dependencies" convention, and the
	// primitives involved (StateField, Decoration.widget) are standard,
	// commonly-used CodeMirror 6 building blocks for exactly this pattern.
	const setGhost = StateEffect.define<string | null>();

	class GhostTextWidget extends WidgetType {
		text: string;
		constructor(text: string) {
			super();
			this.text = text;
		}
		eq(other: GhostTextWidget) {
			return other.text === this.text;
		}
		toDOM() {
			const span = document.createElement('span');
			span.className = 'cm-ghost-text';
			span.textContent = this.text;
			span.setAttribute('aria-hidden', 'true');
			return span;
		}
	}

	// The field stores a positioned DecorationSet directly, not a bare
	// string -- computing the widget's position (tr.state.doc.length,
	// the end of the document, since ghost text is only ever set when
	// the cursor is at the end -- see scheduleCompletion) has to happen
	// here, inside update(), where the *current* document length is
	// available. An earlier version of this field stored just the
	// suggestion string and hardcoded the widget at position 0 in a
	// separate `provide` callback that only receives the field's value,
	// not the document -- a real bug (ghost text rendered at the start
	// of the query, not after what was typed), caught by live-browser
	// verification, not by type-checking, since both are type-correct
	// CodeMirror usage.
	const ghostField = StateField.define<DecorationSet>({
		create: () => Decoration.none,
		update(deco, tr) {
			for (const effect of tr.effects) {
				if (effect.is(setGhost)) {
					if (effect.value === null) return Decoration.none;
					const pos = tr.state.doc.length;
					return Decoration.set([Decoration.widget({ widget: new GhostTextWidget(effect.value), side: 1 }).range(pos)]);
				}
			}
			// Any document change or selection move that isn't the ghost
			// effect itself invalidates a stale suggestion -- showing
			// ghost text for text that no longer reflects what's actually
			// typed would be actively misleading, worse than showing
			// nothing.
			if (tr.docChanged || tr.selection) return Decoration.none;
			return deco;
		},
		provide: (field) => EditorView.decorations.from(field)
	});

	function currentGhostText(view: EditorView): string | null {
		let found: string | null = null;
		view.state.field(ghostField).between(0, view.state.doc.length, (_from, _to, deco) => {
			if (deco.spec.widget instanceof GhostTextWidget) found = deco.spec.widget.text;
		});
		return found;
	}

	let completeGeneration = 0;
	let completeTimer: ReturnType<typeof setTimeout> | undefined;
	const completeDebounceMs = 300;

	function scheduleCompletion(view: EditorView) {
		if (completeTimer) clearTimeout(completeTimer);
		const gen = ++completeGeneration;
		const sel = view.state.selection.main;
		const atEnd = sel.empty && sel.head === view.state.doc.length;
		const text = view.state.doc.toString();
		if (!atEnd || text.trim() === '') return;

		completeTimer = setTimeout(async () => {
			let result: { suggestion: string };
			try {
				result = await aiComplete(text, language);
			} catch {
				return; // provider unavailable/slow/erroring -- silently no ghost text, never a user-facing error (task 5's graceful degradation)
			}
			// Stale response guard: the user kept typing (or the
			// component unmounted) while this request was in flight.
			if (gen !== completeGeneration || !view.hasFocus) return;
			const curSel = view.state.selection.main;
			const stillAtEnd =
				curSel.empty && curSel.head === view.state.doc.length && view.state.doc.toString() === text;
			if (!stillAtEnd || !result.suggestion) return;
			view.dispatch({ effects: setGhost.of(result.suggestion) });
		}, completeDebounceMs);
	}

	function acceptGhost(view: EditorView): boolean {
		const ghost = currentGhostText(view);
		if (!ghost) return false;
		const end = view.state.doc.length;
		view.dispatch({
			changes: { from: end, to: end, insert: ghost },
			selection: { anchor: end + ghost.length },
			effects: setGhost.of(null)
		});
		return true;
	}

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
		'.cm-selectionBackground': { backgroundColor: 'color-mix(in srgb, var(--color-accent) 25%, transparent) !important' },
		'.cm-ghost-text': {
			color: 'var(--color-text-faint)',
			// Not user-selectable/editable -- it's a suggestion, not real
			// document content, and must never end up copy-pasted or
			// merged into a selection as if it were part of the query.
			userSelect: 'none',
			pointerEvents: 'none'
		}
	});

	// Reactive dependency on `container` only -- deliberately not on
	// `value`, even though the view's initial doc needs it. Reading
	// `value` normally here would make this effect a dependent of it,
	// and the updateListener below writes `value` on every keystroke to
	// keep the bindable prop in sync -- if that write re-triggered this
	// effect, it would destroy and recreate the *entire* EditorView on
	// every keystroke. That's not just wasteful: a real, confirmed bug
	// found while building task 5's ghost-text completion --
	// `scheduleCompletion`'s debounce timer lives on `view` and gets
	// silently cancelled by this effect's own cleanup
	// (`clearTimeout(completeTimer)`) moments after being set, because
	// the effect re-runs right after the triggering keystroke. Wrapping
	// the initial `value` read in `untrack` breaks that dependency --
	// the effect now only re-runs when `container` itself changes
	// (mount), matching what it actually needs to do.
	$effect(() => {
		if (!container) return;
		const initialValue = untrack(() => value);
		lastEmitted = initialValue;
		view = new EditorView({
			doc: initialValue,
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
				ghostField,
				keymap.of([
					// Tab accepts ghost text when one is showing -- checked
					// first, ahead of completionKeymap/defaultKeymap, so it
					// never competes with the deterministic dropdown's own
					// Tab/Enter handling for which one wins; the two
					// mechanisms are mutually exclusive at any given moment
					// in practice (a dropdown being open is itself a
					// docChanged-adjacent state ghost text's own
					// invalidation logic tends to have already cleared).
					{ key: 'Tab', run: acceptGhost },
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
						if (aiCompleteEnabled) scheduleCompletion(update.view);
					}
				})
			]
		});
		return () => {
			if (completeTimer) clearTimeout(completeTimer);
			view?.destroy();
		};
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
