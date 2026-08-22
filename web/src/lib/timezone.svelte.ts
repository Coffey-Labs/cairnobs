// Which timezone the UI renders timestamps in. Display only -- see
// $lib/time.ts's header for why that distinction is the whole feature.
//
// Where the choice is *stored* depends on the deployment, and the three
// cases are genuinely different products rather than one with fallbacks:
//
//   - A public demo (isPublicDemo) keeps it in sessionStorage, so every
//     new session starts at UTC again. A shared demo account is used by
//     strangers who have nothing to do with each other; one visitor's
//     choice following the next one around would be a bug, not a
//     feature.
//   - A deployment with local login stores it server-side, per named
//     user (PUT /auth/timezone), so it follows that person across
//     browsers and survives logout -- the setting belongs to the
//     account, not the machine.
//   - Anything else (SSO-only, or no auth configured) has no per-user
//     record to write to, so it falls back to localStorage: still
//     persistent, just per-browser.
import { browser } from '$app/environment';
import { isPublicDemo, localAuthEnabled, setDisplayTimezone } from '$lib/api';

export const DEFAULT_ZONE = 'UTC';

const STORAGE_KEY = 'cairnobs.timezone';

type Persistence = 'session' | 'account' | 'browser';

export function persistence(): Persistence {
	if (isPublicDemo) return 'session';
	return localAuthEnabled ? 'account' : 'browser';
}

function storage(): Storage | null {
	if (!browser) return null;
	try {
		return persistence() === 'session' ? sessionStorage : localStorage;
	} catch {
		// Storage can throw outright in some privacy modes, not just come
		// back empty.
		return null;
	}
}

function readStored(): string | null {
	try {
		return storage()?.getItem(STORAGE_KEY) ?? null;
	} catch {
		return null;
	}
}

let zone = $state<string>(DEFAULT_ZONE);

// Account-mode deployments learn the real value from GET /auth/session,
// which the layout's route guard already fetches on every navigation --
// so this is initialized from that response rather than by issuing a
// second request of its own.
export function initTimezone(fromSession?: string | null) {
	if (persistence() === 'account') {
		zone = fromSession || DEFAULT_ZONE;
		return;
	}
	zone = readStored() || DEFAULT_ZONE;
}

export function getTimezone(): string {
	return zone;
}

/**
 * Applies a zone immediately and persists it wherever this deployment
 * keeps it. The UI updates on the local assignment, not on the server
 * round trip: a failed PUT shouldn't leave someone staring at a control
 * that appears not to respond, and the consequence of the failure is
 * only that the choice won't survive their next login.
 */
export async function setTimezone(tz: string): Promise<void> {
	zone = tz;
	if (persistence() === 'account') {
		await setDisplayTimezone(tz);
		return;
	}
	try {
		storage()?.setItem(STORAGE_KEY, tz);
	} catch {
		// Storage unavailable -- the choice just won't outlive the page.
	}
}

/** The zone this browser thinks it's in, e.g. "Europe/Berlin". */
export function browserTimezone(): string {
	try {
		return Intl.DateTimeFormat().resolvedOptions().timeZone || DEFAULT_ZONE;
	} catch {
		return DEFAULT_ZONE;
	}
}

/**
 * Every IANA zone the browser knows, straight from Intl -- no bundled
 * zone list to go stale as the tz database changes a few times a year.
 * UTC is forced to the front because it's this system's baseline and
 * shouldn't have to be hunted for alphabetically.
 */
export function timezoneOptions(): string[] {
	let all: string[] = [];
	try {
		all = Intl.supportedValuesOf('timeZone');
	} catch {
		// Older engines without supportedValuesOf: offer the two zones
		// that can be named without a list -- the baseline and this
		// browser's own -- rather than nothing.
		all = [browserTimezone()];
	}
	return [DEFAULT_ZONE, ...all.filter((z) => z !== DEFAULT_ZONE)];
}
