// Same shape as settings/+page.ts and dashboards/+page.ts: no route
// params, data comes from a client-side fetch (here, credentialed —
// see this route's +page.svelte and $lib/api.ts's listMemberships).
export const prerender = true;
