<script lang="ts">
	import {
		getAuthFeatures,
		enterpriseAuthBase,
		localAuthEnabled,
		getLocalSession,
		listUsers,
		createUser,
		deleteUser,
		resetPassword,
		type AuthFeatures,
		type LocalSession,
		type LocalUser
	} from '$lib/api';
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

	// --- local user management (owner-role only, see api/localauth) ---
	let localSession = $state<LocalSession | 'disabled' | null>(null);
	let users = $state<LocalUser[]>([]);
	let usersLoading = $state(false);
	let usersError = $state('');
	let newUsername = $state('');
	let newPassword = $state('');
	let newRole = $state('editor');
	let creating = $state(false);
	// lastReset holds a just-generated password so it can be shown once
	// (never stored, never recoverable after -- same posture
	// -seed-admin's initial password takes, see cmd/api/main.go).
	let lastReset = $state<{ userId: string; password: string } | null>(null);

	async function loadUsers() {
		if (!localAuthEnabled) return;
		localSession = await getLocalSession();
		if (localSession === 'disabled' || localSession === null || localSession.role !== 'owner') return;
		usersLoading = true;
		usersError = '';
		try {
			users = await listUsers();
		} catch (e) {
			usersError = e instanceof Error ? e.message : String(e);
		} finally {
			usersLoading = false;
		}
	}
	loadUsers();

	async function handleCreate(e: SubmitEvent) {
		e.preventDefault();
		if (creating) return;
		creating = true;
		usersError = '';
		try {
			await createUser(newUsername, newPassword, newRole);
			newUsername = '';
			newPassword = '';
			newRole = 'editor';
			await loadUsers();
		} catch (e) {
			usersError = e instanceof Error ? e.message : String(e);
		} finally {
			creating = false;
		}
	}

	async function handleDelete(id: string) {
		usersError = '';
		try {
			await deleteUser(id);
			await loadUsers();
		} catch (e) {
			usersError = e instanceof Error ? e.message : String(e);
		}
	}

	async function handleReset(id: string) {
		usersError = '';
		lastReset = null;
		try {
			const { password } = await resetPassword(id);
			if (password) lastReset = { userId: id, password };
		} catch (e) {
			usersError = e instanceof Error ? e.message : String(e);
		}
	}

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

	{#if localAuthEnabled && localSession && localSession !== 'disabled'}
		<section>
			<h2>Users</h2>
			{#if localSession.role !== 'owner'}
				<p class="note">Only an owner can manage users. Signed in as {localSession.username} ({localSession.role}).</p>
			{:else}
				{#if usersError}<p class="error">{usersError}</p>{/if}
				{#if usersLoading}
					<p class="muted">Loading…</p>
				{:else}
					<table>
						<thead>
							<tr>
								<th>Username</th>
								<th>Role</th>
								<th></th>
							</tr>
						</thead>
						<tbody>
							{#each users as u (u.id)}
								<tr>
									<td>{u.username}</td>
									<td class="role-cell">{u.role}</td>
									<td class="actions">
										<button type="button" onclick={() => handleReset(u.id)}>Reset password</button>
										<button type="button" class="danger" onclick={() => handleDelete(u.id)}>Delete</button>
									</td>
								</tr>
								{#if lastReset?.userId === u.id}
									<tr>
										<td colspan="3">
											<p class="note">
												New password (shown once): <code>{lastReset.password}</code>
											</p>
										</td>
									</tr>
								{/if}
							{/each}
						</tbody>
					</table>
				{/if}

				<form onsubmit={handleCreate} class="create-user">
					<input type="text" placeholder="Username" bind:value={newUsername} disabled={creating} required />
					<input
						type="password"
						placeholder="Password (min. 8 characters)"
						bind:value={newPassword}
						disabled={creating}
						required
					/>
					<select bind:value={newRole} disabled={creating}>
						<option value="viewer">Viewer</option>
						<option value="editor">Editor</option>
						<option value="admin">Admin</option>
						<option value="owner">Owner</option>
					</select>
					<button type="submit" disabled={creating}>{creating ? 'Adding…' : 'Add user'}</button>
				</form>
			{/if}
		</section>
	{/if}

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

	table {
		width: 100%;
		border-collapse: collapse;
		margin-bottom: var(--space-4);
		font-size: var(--text-sm);
	}
	th,
	td {
		text-align: left;
		padding: var(--space-2) var(--space-2);
		border-bottom: 1px solid var(--color-border);
	}
	.role-cell {
		color: var(--color-text-muted);
		text-transform: capitalize;
	}
	.actions {
		display: flex;
		gap: var(--space-2);
		justify-content: flex-end;
	}
	.actions button {
		font-size: var(--text-xs);
		padding: var(--space-1) var(--space-2);
		background: var(--color-surface);
		border: 1px solid var(--color-border);
		border-radius: var(--radius-sm);
		color: var(--color-text);
		cursor: pointer;
	}
	.actions button.danger {
		color: var(--color-danger);
		border-color: var(--color-danger);
	}
	.create-user {
		display: flex;
		gap: var(--space-2);
		flex-wrap: wrap;
		align-items: center;
	}
	.create-user input,
	.create-user select {
		font-family: var(--font-ui);
		font-size: var(--text-sm);
		color: var(--color-text);
		background: var(--color-surface);
		border: 1px solid var(--color-border);
		border-radius: var(--radius-sm);
		padding: var(--space-2) var(--space-3);
	}
	.create-user button {
		padding: var(--space-2) var(--space-4);
		font-family: var(--font-ui);
		font-size: var(--text-sm);
		font-weight: var(--font-weight-medium);
		color: var(--color-bg);
		background: var(--color-accent);
		border: none;
		border-radius: var(--radius-sm);
		cursor: pointer;
	}
	.create-user button:disabled {
		cursor: default;
		opacity: 0.6;
	}
	.error {
		color: var(--color-danger);
		font-size: var(--text-sm);
	}
	code {
		font-family: var(--font-mono);
		background: var(--color-surface);
		border: 1px solid var(--color-border);
		border-radius: 3px;
		padding: 0.1rem 0.4rem;
	}
</style>
