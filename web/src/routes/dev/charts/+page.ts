// Not linked from the app's nav or command palette -- a living fixture
// page for verifying the chart layer (rendering, theme/density
// reactivity, and performance at realistic data volumes) without a live
// backend. Kept in the repo rather than thrown away after Phase 5's
// build pass: any future chart change can be sanity-checked here first.
export const prerender = true;
