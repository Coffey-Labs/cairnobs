// Turns a clicked chart data point back into the raw-log query that
// produced it -- the "click to drill into query" affordance. Building
// this generally (rather than special-casing one panel's query) means
// stripping any aggregation stage rather than trying to parse/rewrite
// arbitrary pipe syntax: a panel's query is typically
// `service=api status>=500 | stats count by host, timestamp`, and the
// aggregated `count` a chart point represents doesn't exist as a real
// log row -- the useful drill-down is "show me the raw rows that fed
// this bucket", which means the *pre-aggregation* filter plus whatever
// grouping value was clicked, not the full original query.

export type DrillDownTarget = { query: string; earliest?: string; latest?: string };

const STATS_STAGE = /\|\s*stats\b/i;

function baseFilterQuery(query: string): string {
	const idx = query.search(STATS_STAGE);
	return (idx >= 0 ? query.slice(0, idx) : query).trim();
}

// Quotes a value for use as a bare `field="value"` filter term --
// query-language string literals are double-quoted with backslash
// escapes, same convention field=value filters already use.
function quote(value: string): string {
	return `"${value.replace(/\\/g, '\\\\').replace(/"/g, '\\"')}"`;
}

export function buildDrillDownQuery(
	panelQuery: string,
	point: { seriesName?: string; seriesColumn?: string; xValue: number | string; isTime: boolean; bucketMs?: number }
): DrillDownTarget {
	let query = baseFilterQuery(panelQuery);

	if (point.seriesColumn && point.seriesName) {
		query = `${query} ${point.seriesColumn}=${quote(point.seriesName)}`.trim();
	}

	if (!point.isTime) {
		// Non-time x axis (e.g. grouped by host) -- nothing more to add,
		// the series filter above (or the x value itself, if there's no
		// separate series column) already narrows it enough.
		return { query };
	}

	const center = typeof point.xValue === 'number' ? point.xValue : Date.parse(String(point.xValue));
	if (Number.isNaN(center)) return { query };

	// Half the bucket width on each side when known (a clicked bar/point
	// represents that whole bucket); otherwise a flat 5-minute window --
	// wide enough to catch a clicked point's neighborhood without
	// silently becoming "show me the whole day" like the default range
	// would.
	const halfWindowMs = point.bucketMs ? point.bucketMs / 2 : 5 * 60_000;
	const earliest = new Date(center - halfWindowMs).toISOString();
	const latest = new Date(center + halfWindowMs).toISOString();
	return { query, earliest, latest };
}

// Builds the URL the Search page's drill-down effect reads
// (?q=&earliest=&latest=) -- kept separate from buildDrillDownQuery so
// callers that already have a DrillDownTarget from elsewhere (not just
// a chart click) can link to it too.
export function drillDownUrl(target: DrillDownTarget): string {
	const params = new URLSearchParams({ q: target.query });
	if (target.earliest) params.set('earliest', target.earliest);
	if (target.latest) params.set('latest', target.latest);
	return `/search?${params.toString()}`;
}
