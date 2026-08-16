<script lang="ts">
	import { getAuthFeatures, enterpriseAuthBase, type AuthFeatures } from '$lib/api';
	import { getTheme, setTheme, type Theme } from '$lib/theme.svelte';
	import { getDensity, setDensity, type Density } from '$lib/density.svelte';

	let loading = $state(true);
	let features = $state<AuthFeatures>({ sso_configured: false, oidc_enabled: false, saml_enabled: false });

	async function load() {
		loading = true;
		features = await getAuthFeatures();
		loading = false;
	}
	load();

	const themeOptions: { value: Theme; label: string; hint: string }[] = [
		{ value: 'dark', label: 'Dark', hint: 'Default' },
		{ value: 'light', label: 'Light', hint: '' },
		{ value: 'system', label: 'System', hint: 'Follow OS setting' }
	];
	const densityOptions: { value: Density; label: string; hint: string }[] = [
		{ value: 'comfortable', label: 'Comfortable', hint: 'Dashboards, forms' },
		{ value: 'compact', label: 'Compact', hint: 'Log tables, results' }
	];
</script>

<main>
	<h1>Settings</h1>

	<section>
		<h2>Appearance</h2>
		<div class="option-group" role="radiogroup" aria-label="Theme">
			{#each themeOptions as opt (opt.value)}
				<button
					type="button"
					class="option"
					class:selected={getTheme() === opt.value}
					aria-pressed={getTheme() === opt.value}
					onclick={() => setTheme(opt.value)}
				>
					<span class="option-label">{opt.label}</span>
					{#if opt.hint}<span class="option-hint">{opt.hint}</span>{/if}
				</button>
			{/each}
		</div>
		<div class="option-group" role="radiogroup" aria-label="Row density">
			{#each densityOptions as opt (opt.value)}
				<button
					type="button"
					class="option"
					class:selected={getDensity() === opt.value}
					aria-pressed={getDensity() === opt.value}
					onclick={() => setDensity(opt.value)}
				>
					<span class="option-label">{opt.label}</span>
					<span class="option-hint">{opt.hint}</span>
				</button>
			{/each}
		</div>
	</section>

	<section>
		<h2>Core</h2>
		<p>Single-tenant deployment settings live here. Nothing configurable yet.</p>
	</section>

	{#if loading}
		<p class="muted">Loading…</p>
	{:else if features.sso_configured}
		<section>
			<h2>Single sign-on</h2>
			<ul>
				{#if features.oidc_enabled}<li>OIDC configured</li>{/if}
				{#if features.saml_enabled}<li>SAML configured</li>{/if}
			</ul>
			<p class="note">
				User/role management isn't built yet -- an Admin or Owner will manage tenant members here once
				it is.
			</p>
		</section>
	{:else if enterpriseAuthBase}
		<section>
			<h2>Single sign-on</h2>
			<p class="note">enterprise-auth is deployed but no SSO provider is configured yet.</p>
		</section>
	{/if}
	<!-- No section at all when enterpriseAuthBase is unset -- a Phase 0-3-style
	     single-tenant deployment with no enterprise/ deployed sees a settings
	     page with core content only, no dead "upgrade to unlock" link. See
	     /docs/phase-4-rbac-design.md's "Web UI boundary" section. -->
</main>

<style>
	main {
		max-width: 40rem;
	}
	h1 {
		font-size: var(--text-xl);
		margin-bottom: var(--space-5);
	}
	section {
		margin-bottom: var(--space-6);
	}
	h2 {
		font-size: var(--text-md);
		margin-bottom: var(--space-3);
	}
	.muted {
		color: var(--color-text-muted);
	}
	.note {
		color: var(--color-text-muted);
		font-size: var(--text-sm);
	}
	.option-group {
		display: flex;
		gap: var(--space-2);
		margin-bottom: var(--space-3);
	}
	.option {
		flex: 1;
		display: flex;
		flex-direction: column;
		align-items: flex-start;
		gap: var(--space-1);
		padding: var(--space-3);
		border: 1px solid var(--color-border);
		border-radius: var(--radius-md);
		background: var(--color-surface);
		cursor: pointer;
		text-align: left;
	}
	.option:hover {
		border-color: var(--color-border-strong);
	}
	.option.selected {
		border-color: var(--color-accent);
		background: color-mix(in srgb, var(--color-accent) 8%, var(--color-surface));
	}
	.option-label {
		font-weight: var(--font-weight-medium);
		color: var(--color-text);
	}
	.option-hint {
		font-size: var(--text-xs);
		color: var(--color-text-muted);
	}
	/* The accent-tinted .selected background is *lighter* than plain
	   --color-surface, which quietly drops --color-text-muted below
	   4.5:1 (axe-core's color-contrast check caught this at 4.4:1) --
	   fixed by using the full-strength --color-text on that lighter
	   background specifically, not by changing the shared muted token
	   everywhere else it still passes correctly. */
	.option.selected .option-hint {
		color: var(--color-text);
	}
</style>
