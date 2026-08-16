// Shapes a QueryResult ({columns, rows} -- the pipe-language's one
// tabular output shape, unchanged since Phase 2) into what a chart
// needs. No backend/query-language change was needed for multi-series
// output: a `stats count by service, timestamp`-style query already
// returns "long" rows (one row per service+timestamp pair) -- pivoting
// that into one series per distinct `service` value is frontend work,
// the same way PanelViz already turned {columns, rows} into a single
// uPlot series in Phase 3. viz_config's series_column key (new, but
// viz_config was already an opaque Record<string,string> the backend
// just stores/returns -- see Panel.viz_config -- so this needed no
// schema change either) tells the pivot which column to group by.

// Deliberately narrow: matches ingest's own emitted format
// ("2026-08-13T20:20:24..."), not a general ISO-8601 parser. See
// PanelViz.svelte's original doc comment on why Date.parse() alone is
// too lenient to use as a "does this look like a timestamp" check.
const isoTimestampPrefix = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}/;

export function parseTimeValue(v: unknown): number | null {
	if (typeof v === 'number') return v * 1000;
	if (typeof v !== 'string' || !isoTimestampPrefix.test(v)) return null;
	const parsed = Date.parse(v);
	return Number.isNaN(parsed) ? null : parsed;
}

export type PivotedSeries = {
	name: string;
	data: [number | string, number][];
};

export type Pivoted = {
	isTime: boolean;
	categories: string[]; // populated when !isTime -- category axis labels in row order
	series: PivotedSeries[];
};

function columnIndex(columns: string[], name: string | undefined, fallback: number): number {
	if (!name) return fallback;
	const i = columns.indexOf(name);
	return i >= 0 ? i : fallback;
}

export function pivot(
	columns: string[],
	rows: unknown[][],
	config: { xColumn?: string; valueColumn?: string; seriesColumn?: string }
): Pivoted {
	if (columns.length === 0 || rows.length === 0) {
		return { isTime: false, categories: [], series: [] };
	}

	const xIdx = columnIndex(columns, config.xColumn, 0);
	const seriesIdx = config.seriesColumn ? columns.indexOf(config.seriesColumn) : -1;
	// Value column default: first numeric-looking column that isn't x/series.
	// findIndex returns -1 (not null/undefined) when nothing matches, so a
	// `??` fallback here never fires -- must check for -1 explicitly. This
	// is the only-one-column case (e.g. a bare `stats count`, single_stat's
	// most common query shape): xIdx defaults to 0, excluding the sole
	// column from the search, so findIndex always returns -1 and the value
	// silently read as `undefined` -> 0 without this check.
	const foundValueIdx = columns.findIndex((_, i) => i !== xIdx && i !== seriesIdx && typeof rows[0][i] !== 'string');
	const valueIdx =
		config.valueColumn && columns.includes(config.valueColumn)
			? columns.indexOf(config.valueColumn)
			: foundValueIdx !== -1
				? foundValueIdx
				: (columns.length > 1 ? 1 : xIdx);

	const isTime = rows.every((r) => parseTimeValue(r[xIdx]) !== null);
	const categories: string[] = [];
	const seen = new Map<string, PivotedSeries>();

	for (const row of rows) {
		const xRaw = row[xIdx];
		const x: number | string = isTime ? (parseTimeValue(xRaw) as number) : String(xRaw ?? '');
		if (!isTime && !categories.includes(String(x))) categories.push(String(x));

		const seriesName = seriesIdx >= 0 ? String(row[seriesIdx] ?? '') : columns[valueIdx];
		let s = seen.get(seriesName);
		if (!s) {
			s = { name: seriesName, data: [] };
			seen.set(seriesName, s);
		}
		const raw = row[valueIdx];
		const value = typeof raw === 'number' ? raw : Number(raw) || 0;
		s.data.push([x, value]);
	}

	return { isTime, categories, series: [...seen.values()] };
}
