<script lang="ts">
	import '$lib/styles/app.css';
	import favicon from '$lib/assets/favicon.svg';
	import NavSidebar from '$lib/components/NavSidebar.svelte';
	import CommandPalette from '$lib/components/CommandPalette.svelte';
	import { page } from '$app/state';
	import { getLocalSession } from '$lib/api';

	let { children } = $props();
	let paletteOpen = $state(false);
	let mobileNavOpen = $state(false);

	const isLoginPage = $derived(page.url.pathname === '/login');

	// Gates rendering of {@render children()} entirely until the first
	// auth check resolves -- set once, on the very first check, never
	// reset by later ones (see the $effect below). Without this, the
	// shell (and every child page's own onMount/effect data-fetching)
	// mounted immediately on every navigation, so a protected page's real
	// content -- and a first request for it, before the redirect below
	// even fired -- was visible for one frame on every load. Login-page
	// visits and "local auth isn't configured on this deployment" both
	// count as immediately checked -- neither has anything to gate.
	let initialCheckDone = $state(false);
	let authorized = $state(false);

	// Route guard for local login (see api/localauth's package doc
	// comment) -- client-only, same "no +page.ts/hooks.server.ts load,
	// everything client-fetched" posture every other data-dependent page
	// in this app already uses (this is a prerendered static SPA;
	// checking during SvelteKit's build-time prerender pass would mean
	// fetching a live endpoint at build time, which nothing else here
	// does). getLocalSession() returning 'disabled' means this
	// deployment has no local auth configured at all -- same as a
	// deployment with neither enterprise-auth nor local auth turned on,
	// let every route through unchanged. Re-runs on every navigation
	// ($effect re-fires when isLoginPage's dependency, page.url, changes),
	// same "poll on every navigation" posture GET /auth/session's own
	// doc comment (api/localauth/handler.go) describes -- but only ever
	// sets initialCheckDone, never clears it, so a session expiring
	// mid-use redirects without re-blanking an already-rendered page (a
	// full navigation to /login is already underway by the time that'd
	// matter anyway).
	$effect(() => {
		if (isLoginPage) {
			authorized = true;
			initialCheckDone = true;
			return;
		}
		getLocalSession().then((session) => {
			if (session === null) {
				const next = encodeURIComponent(page.url.pathname + page.url.search);
				window.location.href = `/login?next=${next}`;
			} else {
				authorized = true;
			}
			initialCheckDone = true;
		});
	});
</script>

<svelte:head>
	<title>Cairn OBS</title>
	<link rel="icon" href={favicon} />
	<link rel="icon" type="image/png" sizes="16x16" href="/icons/favicon-16.png" />
	<link rel="icon" type="image/png" sizes="32x32" href="/icons/favicon-32.png" />
	<link rel="icon" type="image/png" sizes="48x48" href="/icons/favicon-48.png" />
	<link rel="apple-touch-icon" sizes="180x180" href="/icons/favicon-180.png" />
</svelte:head>

{#if isLoginPage}
	{@render children()}
{:else if initialCheckDone && authorized}
	<div class="shell">
		<NavSidebar
			onOpenPalette={() => (paletteOpen = true)}
			mobileOpen={mobileNavOpen}
			onCloseMobile={() => (mobileNavOpen = false)}
		/>
		<div class="main-col">
			<button type="button" class="menu-toggle" onclick={() => (mobileNavOpen = true)} aria-label="Open menu">
				☰
			</button>
			<div class="page">
				{@render children()}
			</div>
		</div>
	</div>

	<CommandPalette bind:open={paletteOpen} />
{:else}
	<!-- initialCheckDone is false: the auth check is still in flight (or
	     we're already navigating away to /login) -- nothing protected has
	     mounted yet, this is the whole point. A brief blank screen with no
	     feedback could look frozen on a slow connection, so show the same
	     plain "Loading…" text every other data-dependent page in this app
	     already uses (see e.g. settings/+page.svelte) rather than nothing
	     at all. -->
	<p class="auth-loading">Loading…</p>
{/if}

<style>
	.auth-loading {
		color: var(--color-text-muted);
		padding: var(--space-6);
	}
	.shell {
		display: grid;
		grid-template-columns: 15rem 1fr;
		min-height: 100vh;
	}
	.main-col {
		min-width: 0;
	}
	.page {
		padding: var(--space-6);
		min-width: 0;
	}
	.menu-toggle {
		display: none;
	}

	@media (max-width: 860px) {
		.shell {
			grid-template-columns: 1fr;
		}
		.menu-toggle {
			display: block;
			position: sticky;
			top: 0;
			z-index: 10;
			width: 100%;
			text-align: left;
			background: var(--color-surface);
			border: none;
			border-bottom: 1px solid var(--color-border);
			color: var(--color-text);
			font-size: var(--text-md);
			padding: var(--space-3) var(--space-4);
			cursor: pointer;
		}
		.page {
			padding: var(--space-4);
		}
	}
</style>
