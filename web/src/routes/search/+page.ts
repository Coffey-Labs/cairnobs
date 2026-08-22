// Static adapter needs every route prerenderable. This page has no load
// function (all data comes from a client-side fetch on submit), so a plain
// prerender is enough — no need to disable SSR.
export const prerender = true;
