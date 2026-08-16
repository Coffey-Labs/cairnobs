<script lang="ts">
	import '$lib/styles/app.css';
	import favicon from '$lib/assets/favicon.svg';
	import NavSidebar from '$lib/components/NavSidebar.svelte';
	import CommandPalette from '$lib/components/CommandPalette.svelte';

	let { children } = $props();
	let paletteOpen = $state(false);
	let mobileNavOpen = $state(false);
</script>

<svelte:head>
	<title>Sentry</title>
	<link rel="icon" href={favicon} />
</svelte:head>

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

<style>
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
