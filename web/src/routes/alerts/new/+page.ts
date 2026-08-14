// A static path segment ("new"), not a dynamic route -- SvelteKit
// resolves this before matching /alerts/[id], so "new" never collides
// with a rule ID lookup. No route params, prerenderable like the list page.
export const prerender = true;
