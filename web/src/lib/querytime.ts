// Turning what a human typed into a time range the query API accepts.
//
// The API's rule (api/internal/querylang) is strict and unforgiving of
// human input: an absolute time must be quoted AND carry an explicit
// offset. `earliest="2026-08-22T10:00:00Z"` works; the same value
// unquoted is a syntax error, and with the offset left off it's
// "invalid absolute timestamp ... want RFC3339". So someone reading logs
// in America/Denver who wants "10am today" has to do the arithmetic into
// UTC in their head and remember the quotes -- which is exactly the
// human error worth designing out.
//
// What this does instead: a time typed WITHOUT an offset is read as a
// wall-clock time in the reader's display timezone and converted to the
// UTC instant it refers to, then quoted. A time typed WITH an offset
// (including Z) is taken at its word and only quoted. Relative
// expressions (-1h, now) are left completely alone -- they're already
// zone-independent.
//
// This widens what's accepted rather than reinterpreting anything: every
// naive form handled here is one the API rejects outright today, so no
// query that works now can change meaning. And because the conversion
// happens before the query is sent or saved, what lands in a stored
// dashboard is an explicit UTC instant -- two people in two zones open
// that dashboard and see the same window, not their own local 10am.
import { offsetMinutes } from '$lib/time';

// Date, optionally followed by a time -- the shapes people actually
// type. A bare date means midnight, the same assumption every log tool
// makes for "from the 22nd".
const naiveDateTime = /^(\d{4})-(\d{2})-(\d{2})(?:[T ](\d{2}):(\d{2})(?::(\d{2}))?(\.\d+)?)?$/;

/** True for values that already pin an instant: Z or ±HH:MM. */
function hasExplicitOffset(value: string): boolean {
	return /(?:Z|[+-]\d{2}:?\d{2})$/.test(value);
}

/** True for relative/keyword forms the query language resolves itself. */
function isRelative(value: string): boolean {
	return value === 'now' || /^-\d+[smhdw]$/.test(value);
}

/**
 * Reads `input` as a wall-clock time in `zone` and returns the UTC
 * instant it names, as RFC3339 with a Z. Null if it isn't a naive
 * date/time.
 *
 * The two-pass offset lookup is the standard fix for a real trap: the
 * offset to apply depends on the instant, and the instant is what we're
 * solving for. Guessing the offset from the wall time read as UTC lands
 * within a day of the answer -- close enough that a second lookup at
 * that candidate instant lands on the right side of any DST boundary.
 *
 * Two local times can't be resolved unambiguously by anyone: the hour
 * repeated when clocks go back (this picks one) and the hour skipped
 * when they go forward (this yields the instant the clock jumps to).
 * Both are inherent to naming an instant by wall clock, not a defect of
 * this conversion -- typing an explicit offset sidesteps them.
 */
export function naiveToUTC(input: string, zone: string): string | null {
	const m = naiveDateTime.exec(input.trim());
	if (!m) return null;
	const [, y, mo, d, h = '00', mi = '00', s = '00', frac = ''] = m;
	const ms = frac ? Math.round(parseFloat(frac) * 1000) : 0;
	const asIfUTC = Date.UTC(+y, +mo - 1, +d, +h, +mi, +s, ms);
	if (Number.isNaN(asIfUTC)) return null;

	const firstPass = asIfUTC - offsetMinutes(zone, new Date(asIfUTC)) * 60_000;
	const secondPass = asIfUTC - offsetMinutes(zone, new Date(firstPass)) * 60_000;
	return new Date(secondPass).toISOString();
}

/**
 * What to put after `earliest=` / `latest=` for a value a human typed
 * into a time-range field.
 *
 * Also fixes a bug that predates the timezone work: absolute values were
 * injected unquoted, which the parser rejects outright -- so zooming a
 * dashboard's time-series chart (which feeds an ISO string straight into
 * the range picker) produced a syntax error on every panel.
 */
export function toQueryTimeValue(input: string, zone: string): string {
	const raw = input.trim();
	if (!raw) return raw;

	// Unwrap an already-quoted value so the same rules apply to it, and
	// remember to put the quotes back.
	const quoted = /^"(.*)"$/.exec(raw) ?? /^'(.*)'$/.exec(raw);
	const value = quoted ? quoted[1] : raw;

	if (isRelative(value)) return value;
	if (hasExplicitOffset(value)) return `"${value}"`;

	const utc = naiveToUTC(value, zone);
	if (utc) return `"${utc}"`;

	// Not something we recognize -- pass it through untouched and let the
	// API's own error message be the one the user sees, rather than
	// inventing a second opinion here.
	return raw;
}

// Matches an earliest=/latest= clause and its value: quoted, or bare.
//
// The bare form deliberately reaches across a single space to pick up a
// following clock time, so `earliest=2026-08-22 09:00` is one value
// rather than a date plus a stray `09:00` left dangling in the query --
// which is precisely how someone types a date and time by hand. The
// trailing group only matches something shaped like a time, so
// `earliest=2026-08-22 service=api` still ends the value at the date.
const timeClause =
	/\b(earliest|latest)\s*=\s*("[^"]*"|'[^']*'|[^\s|]+(?:[ ]\d{2}:\d{2}(?::\d{2})?(?:\.\d+)?)?)/g;

/**
 * Rewrites the time clauses inside a query someone typed by hand, the
 * same way the range picker's fields are rewritten.
 *
 * Only for pipe-syntax queries: the raw-SQL escape hatch is passed to
 * ClickHouse verbatim, and rewriting anything inside it would be this
 * layer inventing SQL semantics it has no business having an opinion on.
 */
export function normalizeQueryTimes(query: string, zone: string): string {
	return query.replace(timeClause, (whole, field: string, value: string) => {
		const next = toQueryTimeValue(value, zone);
		return next === value ? whole : `${field}=${next}`;
	});
}
