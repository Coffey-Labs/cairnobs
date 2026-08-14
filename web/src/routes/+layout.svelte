<script lang="ts">
	import favicon from '$lib/assets/favicon.svg';

	// Phase 3: dashboards + alerts routes added, so there's now more than
	// one page -- a minimal nav replaces the previous "no nav, one page"
	// layout.
	let { children } = $props();
</script>

<svelte:head>
	<title>Sentry</title>
	<link rel="icon" href={favicon} />
</svelte:head>

<nav>
	<a href="/">Query</a>
	<a href="/dashboards">Dashboards</a>
	<a href="/alerts">Alerts</a>
</nav>

{@render children()}

<style>
	nav {
		font-family: system-ui, sans-serif;
		max-width: 960px;
		margin: 1rem auto 0;
		padding: 0 1rem;
		display: flex;
		gap: 1.25rem;
	}
	nav a {
		color: #06c;
		text-decoration: none;
		font-size: 0.9rem;
	}
	nav a:hover {
		text-decoration: underline;
	}
</style>
