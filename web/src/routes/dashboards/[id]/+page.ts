// The dashboard ID param doesn't exist at build time, so this route can't
// be prerendered like the list page -- served from adapter-static's
// fallback shell (see vite.config.ts) and rendered fully client-side.
export const prerender = false;
export const ssr = false;
