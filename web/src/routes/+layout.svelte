<script lang="ts">
	import '../app.css';
	import type { Snippet } from 'svelte';
	import Shell from '$lib/Shell.svelte';
	import SignIn from '$lib/SignIn.svelte';
	import { live } from '$lib/live.svelte';
	import { page } from '$app/state';

	// The passkey setup page works without a session: it's how one starts.
	const standalone = $derived(page.url.pathname === '/setup');

	let { children }: { children: Snippet } = $props();

	$effect(() => {
		void live.checkSession();
	});
	$effect(() => {
		if (live.session) live.start();
		return () => live.stop();
	});
</script>

{#if standalone}
	{@render children()}
{:else if live.session}
	<Shell>{@render children()}</Shell>
{:else if live.signin}
	<SignIn dev={live.signin.dev} providers={live.signin.providers} />
{:else if live.error}
	<p class="page">{live.error}</p>
{/if}
