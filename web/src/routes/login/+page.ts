// Client-only, same reasoning as select-tenant/+page.ts -- nothing to
// prerender server-side, everything here is a fetch against api's
// /auth/login.
export const prerender = true;
