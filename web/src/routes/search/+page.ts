// Same reasoning as the root page's +page.ts: no load function (all data
// comes from a client-side fetch on submit), so a plain prerender is
// enough for the static adapter.
export const prerender = true;
