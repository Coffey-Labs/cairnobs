// Density is per-user, persisted, and global -- not per-page. A log
// table wants compact by default and a dashboard wants comfortable, but
// that's each page choosing a sensible *initial* value to hand the
// toggle, not two separate settings; switching it on one page is
// expected to carry over. See app.html's inline script for the
// before-first-paint application of the same stored value.

export type Density = 'comfortable' | 'compact';

const STORAGE_KEY = 'sentry.density';

function readStored(): Density {
	if (typeof localStorage === 'undefined') return 'comfortable';
	return localStorage.getItem(STORAGE_KEY) === 'compact' ? 'compact' : 'comfortable';
}

function apply(d: Density) {
	if (typeof document === 'undefined') return;
	document.documentElement.classList.toggle('density-compact', d === 'compact');
}

const initial = readStored();
let density = $state<Density>(initial);
apply(initial);

export function getDensity(): Density {
	return density;
}

export function setDensity(d: Density) {
	density = d;
	apply(d);
	try {
		localStorage.setItem(STORAGE_KEY, d);
	} catch {
		// storage unavailable -- the choice just won't persist across reloads
	}
}

export function toggleDensity() {
	setDensity(density === 'compact' ? 'comfortable' : 'compact');
}
