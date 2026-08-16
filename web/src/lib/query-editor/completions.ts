// Context-sensitive completions for the pipe syntax -- what's offered
// depends on what precedes the cursor (right after `|` -> stage names;
// right after `stats` -> aggregate functions; otherwise -> field
// names), matching /docs/query-language-reference.md's grammar. Real
// structured field names (`timestamp`/`host`/`service`/`severity`/
// `message`/`record_id`) are always offered since they're always valid
// wherever a field name is; a log's own attribute names aren't known
// client-side (they're schema-on-read, per-record), so those aren't
// suggested -- this is a real limitation, not an oversight.
import type { CompletionContext, CompletionResult } from '@codemirror/autocomplete';

const FIELDS = [
	{ label: 'timestamp', type: 'property' },
	{ label: 'host', type: 'property' },
	{ label: 'service', type: 'property' },
	{ label: 'severity', type: 'property' },
	{ label: 'message', type: 'property' },
	{ label: 'record_id', type: 'property' },
	{ label: 'earliest', type: 'property', info: 'e.g. earliest=-1h' },
	{ label: 'latest', type: 'property', info: 'e.g. latest=now' }
];

const STAGES = [
	{ label: 'where', type: 'keyword', info: 'additional filter' },
	{ label: 'stats', type: 'keyword', info: 'aggregate' },
	{ label: 'sort', type: 'keyword', info: 'order results' },
	{ label: 'fields', type: 'keyword', info: 'choose columns' },
	{ label: 'head', type: 'keyword', info: 'first N results' },
	{ label: 'tail', type: 'keyword', info: 'last N results' }
];

const STATS_FUNCTIONS = [
	{ label: 'count', type: 'function', info: 'count() or count' },
	{ label: 'sum', type: 'function', info: 'sum(field)' },
	{ label: 'avg', type: 'function', info: 'avg(field)' },
	{ label: 'min', type: 'function', info: 'min(field)' },
	{ label: 'max', type: 'function', info: 'max(field)' }
];

export function pipeCompletions(context: CompletionContext): CompletionResult | null {
	const word = context.matchBefore(/[\w.]*/);
	if (!word) return null;
	if (word.from === word.to && !context.explicit) return null;

	const textBefore = context.state.sliceDoc(0, word.from);
	// Nearest preceding pipe stage keyword, if any, and whether we're
	// still within that same stage (no later `|` between it and the
	// cursor).
	const lastPipe = textBefore.lastIndexOf('|');
	const currentStage = textBefore
		.slice(lastPipe + 1)
		.trim()
		.split(/\s+/)[0]
		?.toLowerCase();

	// Right after a `|` (only whitespace since it, or nothing typed yet
	// this stage) -> offer stage keywords.
	const sincePipe = textBefore.slice(lastPipe + 1);
	if (lastPipe >= 0 && /^\s*$/.test(sincePipe)) {
		return { from: word.from, options: STAGES, validFor: /^\w*$/ };
	}

	if (currentStage === 'stats') {
		return { from: word.from, options: STATS_FUNCTIONS, validFor: /^\w*$/ };
	}

	if (currentStage === 'sort' || currentStage === 'fields' || /\bby\s*$/.test(textBefore)) {
		return { from: word.from, options: FIELDS, validFor: /^[\w.]*$/ };
	}

	// Base search or `where` -- field names are always valid here.
	return { from: word.from, options: FIELDS, validFor: /^[\w.]*$/ };
}
