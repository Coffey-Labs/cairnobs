<script lang="ts">
	// Landing page -- the app's first stop after login, distinct from
	// /search (the old root; every prior link/shortcut/drill-down that
	// used to point at "/" expecting the query page now points at
	// "/search" explicitly, see NavSidebar/CommandPalette/drilldown.ts).
	import logoDark from '$lib/assets/logo-stacked-dark.svg';
	import logoLight from '$lib/assets/logo-stacked-light.svg';
	import { isLight } from '$lib/theme.svelte';
	import { Button } from '$lib/components/ui';
	import DemoNotice from '$lib/components/DemoNotice.svelte';
	import { isPublicDemo } from '$lib/api';

	const shortcuts: { href: string; label: string; hint: string }[] = [
		{ href: '/search', label: 'Search', hint: 'Query logs with filters, free-text, or raw SQL' },
		{ href: '/dashboards', label: 'Dashboards', hint: 'Saved multi-panel views over your queries' },
		{ href: '/agents', label: 'Agents', hint: 'Everything reporting logs, and its config' },
		{ href: '/hosts', label: 'Hosts', hint: 'Per-host log volume and service breakdown' }
	];
</script>

<!-- The demo notice wants three readable columns, which 40rem can't
	give it; widened only in that case so no other deployment's landing
	page changes width. -->
<main class:with-notice={isPublicDemo}>
	<img src={isLight() ? logoLight : logoDark} alt="Cairn OBS" class="logo" />
	<p class="tagline">
		One query bar for filter/stats queries and free-text search across every host and service
		you're shipping logs from.
	</p>

	{#if isPublicDemo}
		<DemoNotice />
	{/if}

	<div class="shortcuts">
		{#each shortcuts as s (s.href)}
			<Button href={s.href} variant="secondary">
				<span class="shortcut-label">{s.label}</span>
				<span class="shortcut-hint">{s.hint}</span>
			</Button>
		{/each}
	</div>
</main>

<style>
	main {
		display: flex;
		flex-direction: column;
		align-items: center;
		text-align: center;
		max-width: 40rem;
		margin: 0 auto;
		padding-top: var(--space-8, 4rem);
	}
	main.with-notice {
		max-width: 48rem;
	}
	.logo {
		/* The stacked lockup is 360x320, not square -- height must stay
		   auto or the wordmark under the cairn stretches. */
		width: 11rem;
		height: auto;
	}
	.tagline {
		margin-top: var(--space-4);
		color: var(--color-text-muted);
		max-width: 32rem;
	}
	.shortcuts {
		margin-top: var(--space-6);
		display: grid;
		grid-template-columns: 1fr 1fr;
		gap: var(--space-3);
		width: 100%;
	}
	.shortcuts :global(.btn) {
		display: flex;
		flex-direction: column;
		align-items: flex-start;
		gap: var(--space-1);
		height: auto;
		padding: var(--space-3) var(--space-4);
		text-align: left;
	}
	.shortcut-label {
		font-weight: var(--font-weight-medium);
	}
	.shortcut-hint {
		font-size: var(--text-xs);
		color: var(--color-text-muted);
		font-weight: var(--font-weight-normal);
	}
	@media (max-width: 30rem) {
		.shortcuts {
			grid-template-columns: 1fr;
		}
	}
</style>
