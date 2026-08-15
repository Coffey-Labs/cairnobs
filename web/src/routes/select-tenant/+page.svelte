<script lang="ts">
	// Phase 4's tenant-picker page: enterprise-auth's finishLogin lands
	// the browser here (SELECT_TENANT_REDIRECT_URL, default
	// http://localhost:3000/select-tenant -- see enterprise/internal/
	// config's doc comment) after a login resolves to more than one
	// tenant_memberships row, carrying a short-lived sentry_pending_login
	// cookie instead of a real session. This page's whole job: show the
	// choices GET /auth/memberships returns, and turn a click into
	// POST /auth/select-tenant, which trades that cookie for a real
	// session and tells us where to go next.
	//
	// No +page.ts -- everything here is client-only (fetch with
	// credentials against a different origin), nothing to prerender or
	// load server-side, same reasoning as every other data-fetching route
	// in this app.
	import { listMemberships, selectTenant, type Membership } from '$lib/api';

	let phase = $state<'loading' | 'ready' | 'error'>('loading');
	let memberships = $state<Membership[]>([]);
	let error = $state('');
	let selectingTenantId = $state('');

	async function load() {
		phase = 'loading';
		error = '';
		try {
			memberships = await listMemberships();
			phase = 'ready';
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
			phase = 'error';
		}
	}
	load();

	async function choose(tenantId: string) {
		selectingTenantId = tenantId;
		error = '';
		try {
			const { redirect_url } = await selectTenant(tenantId);
			// Full navigation, not SvelteKit's router: redirect_url is
			// enterprise-auth's postLoginRedirectURL, i.e. this app's own
			// base URL -- reloading picks up the real session cookie
			// POST /auth/select-tenant just set, which client-side
			// routing wouldn't need to know about but a fresh page load
			// makes unambiguous.
			window.location.href = redirect_url;
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
			selectingTenantId = '';
		}
	}
</script>

<main>
	<h1>Select a workspace</h1>

	{#if phase === 'loading'}
		<p>Loading…</p>
	{:else if phase === 'error' && memberships.length === 0}
		<p class="error">{error}</p>
		<p class="note">Your login link may have expired. Start over by logging in again.</p>
	{:else}
		{#if error}
			<p class="error">{error}</p>
		{/if}
		<ul>
			{#each memberships as m (m.tenant_id)}
				<li>
					<button
						disabled={selectingTenantId !== ''}
						onclick={() => choose(m.tenant_id)}
					>
						<span class="name">{m.tenant_display_name}</span>
						<span class="role">{m.role}</span>
						{#if selectingTenantId === m.tenant_id}<span class="note">Signing in…</span>{/if}
					</button>
				</li>
			{/each}
		</ul>
	{/if}
</main>

<style>
	main {
		font-family: system-ui, sans-serif;
		max-width: 480px;
		margin: 3rem auto;
		padding: 0 1rem;
	}
	ul {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-direction: column;
		gap: 0.5rem;
	}
	button {
		width: 100%;
		display: flex;
		align-items: center;
		gap: 0.75rem;
		padding: 0.75rem 1rem;
		font-size: 1rem;
		text-align: left;
		background: #fff;
		border: 1px solid #ccc;
		border-radius: 6px;
		cursor: pointer;
	}
	button:hover:not(:disabled) {
		border-color: #06c;
	}
	button:disabled {
		cursor: default;
		opacity: 0.6;
	}
	.name {
		flex: 1;
		font-weight: 600;
	}
	.role {
		color: #666;
		font-size: 0.85rem;
		text-transform: capitalize;
	}
	.error {
		color: #b00020;
	}
	.note {
		color: #666;
		font-size: 0.85rem;
	}
</style>
