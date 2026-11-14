// What a map line carries, from its ports, and the colour that says so:
// web blue, mail orange, data green, messaging pink, anything else grey.
// Trouble at the end that's needed overrides it (see each map).

export type Carries = 'web' | 'mail' | 'data' | 'messaging' | 'other';

const PORTS: Record<number, Carries> = {};
const put = (c: Carries, ...ps: number[]) => ps.forEach((p) => (PORTS[p] = c));
put('web', 80, 443, 3000, 5000, 8000, 8008, 8080, 8081, 8443, 8888, 9000, 9090);
put('mail', 25, 110, 143, 465, 587, 993, 995, 4190, 11334);
put('data', 445, 636, 389, 1433, 1521, 2049, 2379, 3306, 5432, 5984, 6379, 6432, 8086, 9042, 9200, 11211, 27017, 27000, 8400);
put('messaging', 1883, 4222, 5671, 5672, 6222, 8883, 9092, 61613, 61616);

/** What a line with these ports carries: the first port that says. */
export function carries(ports: number[] | undefined): Carries {
	for (const p of ports ?? []) {
		const c = PORTS[p];
		if (c) return c;
	}
	return 'other';
}

export const CARRIES_COLOR: Record<Carries, string> = {
	web: 'var(--s1)', mail: 'var(--s2)', data: 'var(--s3)', messaging: 'var(--s5)', other: 'var(--muted-fg)'
};

export const CARRIES_WORD: Record<Carries, string> = {
	web: 'web and APIs', mail: 'mail', data: 'databases, caches and storage', messaging: 'messaging', other: 'other'
};

/** Past this many lines on one view, only the ones pointed at flow. */
export const FLOW_ALL_UP_TO = 300;
