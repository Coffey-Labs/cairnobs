<script lang="ts">
	// Configure is the control plane: admins change it, operators see it
	// read-only. The server refuses its settings to anyone else whatever this
	// page shows.
	import type { Snippet } from 'svelte';
	import { live } from '$lib/live.svelte';

	let { children }: { children: Snippet } = $props();
</script>

{#if live.seesAll}
	{#if !live.admin}<p class="ro">You're an operator: you can see how Cairn OBS is set up here, but only admins change it.</p>{/if}
	{@render children()}
{:else}
	<p class="empty">Only admins and operators can see how Cairn OBS is set up. <a href="/">Back to the dashboard</a>.</p>
{/if}

<style>
	.ro { margin: 0 0 16px; padding: 10px 14px; border: 1px solid var(--border); border-radius: var(--radius); background: var(--muted); font-size: 13px; }
</style>
