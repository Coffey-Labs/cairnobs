// A hand-rolled StreamLanguage tokenizer for the pipe syntax
// (/docs/query-language-reference.md), not a full Lezer grammar --
// the language is small and mostly flat (no nested expressions beyond
// one comparison per term), so a single-pass stream tokenizer covers it
// without the added build complexity a real parser grammar would need.
// Raw SQL (a query starting with SELECT) is deliberately NOT
// highlighted by this -- it's an escape hatch, not the primary UX
// target, and reusing @codemirror/lang-sql for it would be a second
// grammar to maintain for a path most queries don't take.
//
// token()'s return value is looked up directly against @lezer/highlight's
// `tags` export by name (see @codemirror/language's StreamLanguage
// implementation) -- it must be one of those real tag names, not an
// arbitrary string. `where`/`stats`/`sort`/`fields`/`head`/`tail`
// (pipeline-stage keywords) use `controlKeyword`; `and`/`or`/`by`/`as`
// (connective words within a stage) use `operatorKeyword` -- two
// distinct real tags, chosen so the two keyword classes render
// differently without inventing a custom tag StreamLanguage can't
// resolve.
import { StreamLanguage, HighlightStyle, syntaxHighlighting, type StringStream } from '@codemirror/language';
import { tags as t } from '@lezer/highlight';

const STAGE_KEYWORDS = new Set(['where', 'stats', 'sort', 'fields', 'head', 'tail']);
const CONNECTIVES = new Set(['and', 'or', 'by', 'as']);
const STATS_FUNCTIONS = new Set(['count', 'sum', 'avg', 'min', 'max']);
const TIME_FIELDS = new Set(['earliest', 'latest']);

export const pipeLanguage = StreamLanguage.define({
	name: 'cairnobs-pipe',
	startState() {
		return { afterPipe: true };
	},
	token(stream: StringStream, state: { afterPipe: boolean }) {
		if (stream.eatSpace()) return null;

		if (stream.match('|')) {
			state.afterPipe = true;
			return 'punctuation';
		}

		if (stream.peek() === '"') {
			stream.next();
			while (!stream.eol()) {
				if (stream.next() === '"' && stream.string[stream.pos - 2] !== '\\') break;
			}
			return 'string';
		}

		if (stream.match(/^-?\d+(\.\d+)?/)) return 'number';

		if (stream.match(/^(>=|<=|!=|=|>|<)/)) return 'compareOperator';

		if (stream.match(/^[+-](?=\w)/)) return 'compareOperator'; // sort direction sigil

		if (stream.match(/^[A-Za-z_][\w.]*/)) {
			const word = stream.current().toLowerCase();
			const wasAfterPipe = state.afterPipe;
			state.afterPipe = false;
			if (wasAfterPipe && STAGE_KEYWORDS.has(word)) return 'controlKeyword';
			if (CONNECTIVES.has(word)) return 'operatorKeyword';
			if (STATS_FUNCTIONS.has(word) && stream.peek() === '(') return 'name.function';
			if (TIME_FIELDS.has(word)) return 'atom';
			return 'variableName';
		}

		if (stream.match(/^[(),]/)) return 'punctuation';

		stream.next();
		return null;
	}
});

const highlightStyle = HighlightStyle.define([
	{ tag: t.controlKeyword, color: 'var(--color-accent)', fontWeight: '600' },
	{ tag: t.operatorKeyword, color: 'var(--color-sev-info)' },
	{ tag: t.function(t.name), color: 'var(--color-sev-warn)' },
	{ tag: t.atom, color: 'var(--color-sev-info)' },
	{ tag: t.string, color: 'var(--color-sev-quiet)' },
	{ tag: t.number, color: 'var(--color-text)' },
	{ tag: t.compareOperator, color: 'var(--color-sev-error)' },
	{ tag: t.variableName, color: 'var(--color-text)' },
	{ tag: t.punctuation, color: 'var(--color-text-muted)' }
]);

export const pipeSyntaxHighlighting = syntaxHighlighting(highlightStyle);
