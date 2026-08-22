// Theme is dark/light/"system" -- but unlike most apps, an unset
// preference means dark, not system. That's the whole point of "real
// dark mode as the default, not an afterthought": a first-time visitor
// on a light-OS machine still lands in dark, matching the actual usage
// pattern this product is designed for (long sessions, often at night).
// "system" is available for anyone who wants their OS setting to win
// instead, but it's a deliberate opt-in, not the fallback.
//
// The synchronous inline script in app.html applies the same stored
// value before first paint -- this module is what UI controls read/write
// after hydration; the two must stay in sync on the storage key and
// values, not just in spirit.

export type Theme = 'dark' | 'light' | 'system';

const STORAGE_KEY = 'cairnobs.theme';

function readStored(): Theme {
	if (typeof localStorage === 'undefined') return 'dark';
	const v = localStorage.getItem(STORAGE_KEY);
	return v === 'light' || v === 'system' ? v : 'dark';
}

function apply(t: Theme) {
	if (typeof document === 'undefined') return;
	if (t === 'system') {
		document.documentElement.removeAttribute('data-theme');
	} else {
		document.documentElement.setAttribute('data-theme', t);
	}
}

const initial = readStored();
let theme = $state<Theme>(initial);
apply(initial);

// Tracks the OS preference live (not just at load) so "system" stays
// accurate across a theme change the user makes outside the app, same
// as app.css/tokens.css's own `prefers-color-scheme` media query does
// automatically for CSS -- anything in JS that needs to know "is the UI
// actually light right now" (e.g. picking a light/dark logo asset) has
// to track this the same way, or it drifts from what's on screen.
let prefersLight = $state(
	typeof window !== 'undefined' ? window.matchMedia('(prefers-color-scheme: light)').matches : false
);
if (typeof window !== 'undefined') {
	window
		.matchMedia('(prefers-color-scheme: light)')
		.addEventListener('change', (e) => (prefersLight = e.matches));
}

export function getTheme(): Theme {
	return theme;
}

// Whether the rendered UI is in light mode right now -- explicit
// "light", or "system" while the OS itself prefers light. Mirrors
// app.css's `html[data-theme='light']` / `prefers-color-scheme: light`
// precedence exactly.
export function isLight(): boolean {
	return theme === 'light' || (theme === 'system' && prefersLight);
}

export function setTheme(t: Theme) {
	theme = t;
	apply(t);
	try {
		localStorage.setItem(STORAGE_KEY, t);
	} catch {
		// storage unavailable -- the choice just won't persist across reloads
	}
}
