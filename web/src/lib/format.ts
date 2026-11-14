// Times are shown in the viewer's own time zone (the browser's), labelled
// with its short name (MST, CET, GMT+2), with UTC on hover.
const hmFmt = new Intl.DateTimeFormat('en-US', { hour: '2-digit', minute: '2-digit', hour12: false });
const hmsFmt = new Intl.DateTimeFormat('en-US', { hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false });
const hourFmt = new Intl.DateTimeFormat('en-US', { hour: 'numeric', hour12: false });
const zoneFmt = new Intl.DateTimeFormat('en-US', { timeZoneName: 'short' });

export const hm = (t: Date | string | number) => hmFmt.format(new Date(t));
export const hms = (t: Date | string | number) => hmsFmt.format(new Date(t));
export const utc = (t: Date | string | number) => new Date(t).toISOString().slice(11, 16) + ' UTC';
/** The hour of the day where the viewer is. */
export const hour = (t: Date | number = Date.now()) => +hourFmt.format(new Date(t)) % 24;
/** The viewer's time zone's short name at that moment (it changes with daylight saving). */
export const zone = (t: Date | string | number = Date.now()) => zoneFmt.formatToParts(new Date(t)).find((p) => p.type === 'timeZoneName')?.value ?? '';
/** A time of day with its zone: "20:30 MST". */
export const hmz = (t: Date | string | number) => hm(t) + ' ' + zone(t);

export function bytes(n: number): string {
	const units = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];
	let i = 0;
	while (n >= 1024 && i < units.length - 1) {
		n /= 1024;
		i++;
	}
	return (n >= 100 || i === 0 ? Math.round(n) : n.toFixed(1)) + ' ' + units[i];
}

/** Bytes per second as bits per second, the way network speeds are quoted. */
export function rate(bps: number): string {
	const bits = bps * 8;
	if (bits >= 1e9) return (bits / 1e9).toFixed(1) + ' Gb/s';
	if (bits >= 1e6) return (bits / 1e6).toFixed(1) + ' Mb/s';
	if (bits >= 1e3) return Math.round(bits / 1e3) + ' kb/s';
	return Math.round(bits) + ' b/s';
}

export function duration(seconds: number): string {
	const s = Math.max(0, Math.round(seconds));
	const d = Math.floor(s / 86400), h = Math.floor((s % 86400) / 3600), m = Math.floor((s % 3600) / 60);
	if (d) return `${d} d ${h} h`;
	if (h) return `${h} h ${m} min`;
	if (m) return `${m} min`;
	return `${s} s`;
}

export function ago(t: string | number | Date, now = Date.now()): string {
	return duration((now - new Date(t).getTime()) / 1000) + ' ago';
}

export const pct = (n: number) => Math.round(n) + '%';

// sourceLabel says a source key for people: "mgmt1:vcsa-01" is
// "vcsa-01 (mgmt1)", something an agent reads beside its host; a bare
// name is a cluster agent's, or a host's VMs.
export const sourceLabel = (key: string) => {
	const i = key.indexOf(':');
	return i < 0 ? key : `${key.slice(i + 1)} (${key.slice(0, i)})`;
};
