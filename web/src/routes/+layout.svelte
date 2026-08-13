<script lang="ts">
	import favicon from '$lib/assets/favicon.svg';
	import { page } from '$app/state';

	let { children } = $props();
</script>

<svelte:head>
	<title>Sentry</title>
	<link rel="icon" href={favicon} />
</svelte:head>

<nav>
	<a href="/" class:active={page.url.pathname === '/'}>SQL Query</a>
	<a href="/search" class:active={page.url.pathname === '/search'}>Full-Text Search</a>
</nav>

{@render children()}

<style>
	nav {
		font-family: system-ui, sans-serif;
		max-width: 960px;
		margin: 1rem auto 0;
		padding: 0 1rem;
		display: flex;
		gap: 1rem;
		border-bottom: 1px solid #ccc;
	}
	nav a {
		padding: 0.5rem 0;
		text-decoration: none;
		color: #555;
		border-bottom: 2px solid transparent;
	}
	nav a.active {
		color: #000;
		border-bottom-color: #000;
		font-weight: 600;
	}
</style>
