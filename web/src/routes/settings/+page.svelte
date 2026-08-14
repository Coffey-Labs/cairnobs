<script lang="ts">
	import { getAuthFeatures, enterpriseAuthBase, type AuthFeatures } from '$lib/api';

	let loading = $state(true);
	let features = $state<AuthFeatures>({ sso_configured: false, oidc_enabled: false, saml_enabled: false });

	async function load() {
		loading = true;
		features = await getAuthFeatures();
		loading = false;
	}
	load();
</script>

<main>
	<h1>Settings</h1>

	<section>
		<h2>Core</h2>
		<p>Single-tenant deployment settings live here. Nothing configurable yet.</p>
	</section>

	{#if loading}
		<p>Loading…</p>
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
		font-family: system-ui, sans-serif;
		max-width: 960px;
		margin: 2rem auto;
		padding: 0 1rem;
	}
	section {
		margin-bottom: 1.5rem;
	}
	h2 {
		font-size: 1.1rem;
	}
	.note {
		color: #666;
		font-size: 0.85rem;
	}
</style>
