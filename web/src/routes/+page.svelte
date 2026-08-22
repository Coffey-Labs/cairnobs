<script lang="ts">
	// Landing page -- the app's first stop after login, distinct from
	// /search (the old root; every prior link/shortcut/drill-down that
	// used to point at "/" expecting the query page now points at
	// "/search" explicitly, see NavSidebar/CommandPalette/drilldown.ts).
	import logo from '$lib/assets/logo-stacked-dark.svg';
	import { Button } from '$lib/components/ui';

	const shortcuts: { href: string; label: string; hint: string }[] = [
		{ href: '/search', label: 'Search', hint: 'Query logs with filters, free-text, or raw SQL' },
		{ href: '/dashboards', label: 'Dashboards', hint: 'Saved multi-panel views over your queries' },
		{ href: '/agents', label: 'Agents', hint: 'Everything reporting logs, and its config' },
		{ href: '/hosts', label: 'Hosts', hint: 'Per-host log volume and service breakdown' }
	];
</script>

<main>
	<img src={logo} alt="Cairn OBS" class="logo" />
	<p class="tagline">
		One query bar for filter/stats queries and free-text search across every host and service
		you're shipping logs from.
	</p>

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
	.logo {
		width: 11rem;
		height: 11rem;
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
