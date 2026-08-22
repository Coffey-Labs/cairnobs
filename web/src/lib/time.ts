// Timestamp rendering. Pure functions, no state -- the caller passes the
// zone in, and $lib/timezone.svelte.ts owns where that zone comes from.
//
// The contract this whole feature rests on: nothing here ever changes
// *which* instant a value refers to, only how it's written down. The
// data stays UTC end to end (ingest records Unix nanoseconds, ClickHouse
// stores UTC, the API emits RFC3339 with a Z), queries are still
// evaluated in UTC, and two users in two zones looking at one log line
// see the same instant rendered two ways -- never two different lines,
// and never a different sort order.

// Deliberately narrow, matching what this system's own APIs emit:
// RFC3339/ISO-8601 with a date, a T (or space) separator, and a time.
// A fractional part and a zone suffix are both optional because the two
// sources differ -- ClickHouse query results come back like
// "2026-08-22T21:30:06.090041211Z" (nanoseconds), Postgres-backed JSON
// like "2026-08-22T21:30:06.477115Z" (microseconds).
//
// Being narrow is the point: ResultsTable runs this over every cell of
// every column, and a looser pattern would start reformatting values
// that merely resemble dates (a version string, an ID with dashes) and
// silently corrupt them.
const isoTimestamp = /^(\d{4})-(\d{2})-(\d{2})[T ](\d{2}):(\d{2}):(\d{2})(\.\d+)?(Z|[+-]\d{2}:?\d{2})$/;

export function isTimestamp(value: unknown): value is string {
	return typeof value === 'string' && isoTimestamp.test(value);
}

// Intl.DateTimeFormat construction is expensive enough to matter when
// it's called once per cell on a 5,000-row result; the zone rarely
// changes, so one formatter per zone is cached for the page's lifetime.
const formatters = new Map<string, Intl.DateTimeFormat>();

function formatterFor(zone: string): Intl.DateTimeFormat {
	let f = formatters.get(zone);
	if (!f) {
		f = new Intl.DateTimeFormat('en-CA', {
			timeZone: zone,
			hourCycle: 'h23',
			year: 'numeric',
			month: '2-digit',
			day: '2-digit',
			hour: '2-digit',
			minute: '2-digit',
			second: '2-digit'
		});
		formatters.set(zone, f);
	}
	return f;
}

// 'en-CA' yields YYYY-MM-DD natively, but only formatToParts is
// guaranteed to give the pieces without a locale-specific separator
// sneaking in, so the string is assembled by hand.
function parts(value: Date, zone: string): Record<string, string> {
	const out: Record<string, string> = {};
	for (const p of formatterFor(zone).formatToParts(value)) out[p.type] = p.value;
	return out;
}

export type TimestampPrecision = 'seconds' | 'millis' | 'full';

/**
 * Renders an ISO timestamp in `zone` as `YYYY-MM-DD HH:mm:ss[.fff]`.
 *
 * Anything that isn't a recognizable timestamp comes back untouched --
 * this runs over unknown query output, where guessing wrong is worse
 * than doing nothing.
 *
 * Sub-second digits are taken verbatim from the source string rather
 * than from the parsed Date: JS Dates are millisecond-precision, so
 * round-tripping a ClickHouse nanosecond timestamp through one would
 * silently drop six digits of a log's ordering information.
 */
export function formatTimestamp(
	value: unknown,
	zone: string,
	precision: TimestampPrecision = 'millis'
): string {
	if (!isTimestamp(value)) return value === null || value === undefined ? '' : String(value);
	const ms = Date.parse(value);
	if (Number.isNaN(ms)) return value;

	const p = parts(new Date(ms), zone);
	const base = `${p.year}-${p.month}-${p.day} ${p.hour}:${p.minute}:${p.second}`;
	if (precision === 'seconds') return base;

	const fraction = isoTimestamp.exec(value)?.[7] ?? '';
	if (!fraction) return base;
	return base + (precision === 'full' ? fraction : fraction.slice(0, 4));
}

/** Date only, for created-at style columns where the time of day is noise. */
export function formatDate(value: unknown, zone: string): string {
	if (!isTimestamp(value)) return value === null || value === undefined ? '' : String(value);
	const p = parts(new Date(Date.parse(value)), zone);
	return `${p.year}-${p.month}-${p.day}`;
}

/**
 * The UTC offset `zone` is at for a given instant, as `+HH:MM`.
 *
 * Computed for a specific moment, not for the zone in general, because
 * half the world's zones have two answers depending on the date -- a
 * label that says -08:00 in July for America/Los_Angeles is a lie, and
 * the whole point of storing a zone name rather than a fixed offset is
 * that the rules are what matter.
 */
export function offsetLabel(zone: string, at: Date = new Date()): string {
	// 'longOffset' gives "GMT+11:00" directly. The arithmetic
	// alternative -- formatting the same instant in two zones and
	// subtracting -- means re-parsing a locale-formatted string, which is
	// implementation-defined; this asks the platform the question
	// outright instead.
	const name = new Intl.DateTimeFormat('en-US', { timeZone: zone, timeZoneName: 'longOffset' })
		.formatToParts(at)
		.find((p) => p.type === 'timeZoneName')?.value;
	// Zero-offset zones format as a bare "GMT", with no numeric part.
	return /GMT([+-]\d{2}:\d{2})/.exec(name ?? '')?.[1] ?? '+00:00';
}

/**
 * The same offset as a signed number of minutes, for arithmetic --
 * `+05:30` is +330. Kept next to offsetLabel so the two can't disagree
 * about what a zone's offset is.
 */
export function offsetMinutes(zone: string, at: Date = new Date()): number {
	const [, sign, hh, mm] = /^([+-])(\d{2}):(\d{2})$/.exec(offsetLabel(zone, at)) ?? [];
	if (!sign) return 0;
	return (sign === '-' ? -1 : 1) * (Number(hh) * 60 + Number(mm));
}

/** "UTC" / "America/New_York +11:00" -- what a column header or picker shows. */
export function zoneLabel(zone: string, at: Date = new Date()): string {
	return zone === 'UTC' ? 'UTC' : `${zone} ${offsetLabel(zone, at)}`;
}

/**
 * Relative age ("3m ago"). Zone-independent by construction -- the gap
 * between two instants is the same number everywhere on earth -- so it
 * takes no zone argument, and pages that show only relative times need
 * no timezone plumbing at all.
 */
export function relativeTime(value: string, now: number = Date.now()): string {
	const ms = now - Date.parse(value);
	if (Number.isNaN(ms)) return '';
	if (ms < 60_000) return `${Math.max(0, Math.round(ms / 1000))}s ago`;
	if (ms < 3_600_000) return `${Math.round(ms / 60_000)}m ago`;
	if (ms < 86_400_000) return `${Math.round(ms / 3_600_000)}h ago`;
	return `${Math.round(ms / 86_400_000)}d ago`;
}

/**
 * Compact axis label for a time-series chart, rendered in `zone`.
 *
 * ECharts' own `type: 'time'` axis formats in the *browser's* zone with
 * no way to tell it otherwise, which would put a chart's clock an hour
 * or ten out of step with the table right beside it. Every time axis in
 * this app therefore formats its own labels through here.
 *
 * The shape of the label follows the visible span, the way any chart's
 * does: a few hours wants the time of day, a week wants the date.
 */
export function axisTimeLabel(ms: number, zone: string, spanMs: number): string {
	const full = formatTimestamp(new Date(ms).toISOString(), zone, 'seconds');
	if (spanMs > 5 * 86_400_000) return full.slice(0, 10); // YYYY-MM-DD
	if (spanMs > 86_400_000) return full.slice(5, 16); // MM-DD HH:mm
	return full.slice(11, 16); // HH:mm
}
