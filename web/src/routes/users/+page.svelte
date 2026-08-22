<script lang="ts">
	import { getTimezone } from '$lib/timezone.svelte';
	import { formatDate as formatDateInZone } from '$lib/time';
	import {
		localAuthEnabled,
		getLocalSession,
		listUsers,
		createUser,
		deleteUser,
		resetPassword,
		setUserRole,
		type LocalSession,
		type LocalUser
	} from '$lib/api';

	const roleOptions = ['viewer', 'editor', 'admin', 'owner'] as const;
	// An admin caller can only ever create/pick viewer or editor -- the
	// server rejects admin/owner from an admin caller (see
	// api/localauth/handler.go's handleCreateUser), so this is here to
	// keep the create form from ever offering a choice that would 403.
	const adminCreatableRoles = ['viewer', 'editor'] as const;

	let localSession = $state<LocalSession | 'disabled' | null>(null);
	let checked = $state(false);
	let users = $state<LocalUser[]>([]);
	let usersLoading = $state(false);
	let usersError = $state('');

	async function loadUsers() {
		localSession = localAuthEnabled ? await getLocalSession() : 'disabled';
		checked = true;
		if (localSession === 'disabled' || localSession === null) return;
		if (localSession.role !== 'owner' && localSession.role !== 'admin') return;
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

	// isOwner gates what this page offers beyond the owner-or-admin floor
	// that gets you onto the page at all: role reassignment, and
	// creating/deleting/resetting an admin or owner account, all stay
	// owner-only (see api/localauth/handler.go's RBAC matrix doc
	// comment). $derived.by + a local const alias sidesteps a
	// svelte-check narrowing quirk with reading a mutable $state
	// directly inside a bare $derived(...) expression.
	const isOwner = $derived.by(() => {
		const s = localSession;
		return s !== null && s !== 'disabled' && s.role === 'owner';
	});
	const ownerCount = $derived(users.filter((u) => u.role === 'owner').length);

	// canDeleteUser/canResetPassword mirror the server's own checks so
	// the UI never offers an action that would just come back as a 403
	// or (for the last owner) a 409 -- the server remains the actual
	// authority, this is purely a "don't show a doomed button" nicety.
	function canDeleteUser(u: LocalUser): boolean {
		if ((u.role === 'admin' || u.role === 'owner') && !isOwner) return false;
		if (u.role === 'owner' && ownerCount <= 1) return false;
		return true;
	}

	function deleteDisabledReason(u: LocalUser): string {
		if ((u.role === 'admin' || u.role === 'owner') && !isOwner) return 'Only an owner can delete an admin or owner account';
		if (u.role === 'owner' && ownerCount <= 1) return 'There must always be at least one owner';
		return '';
	}

	function canResetPassword(u: LocalUser): boolean {
		return !(u.role === 'owner' && !isOwner);
	}

	// --- create user ---
	let newUsername = $state('');
	let newPassword = $state('');
	let newRole = $state('editor');
	let creating = $state(false);
	let showCreate = $state(false);

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
			showCreate = false;
			await loadUsers();
		} catch (e) {
			usersError = e instanceof Error ? e.message : String(e);
		} finally {
			creating = false;
		}
	}

	async function handleDelete(u: LocalUser) {
		if (!confirm(`Delete ${u.username}? This can't be undone.`)) return;
		usersError = '';
		try {
			await deleteUser(u.id);
			if (passwordTarget === u.id) passwordTarget = null;
			await loadUsers();
		} catch (e) {
			usersError = e instanceof Error ? e.message : String(e);
		}
	}

	// --- role reassignment: saves immediately when the select changes,
	// reverting on failure so the UI never shows a role that didn't
	// actually take (see api/localauth's SetRole doc comment -- a role
	// change also revokes the target's existing sessions). ---
	let roleSaving = $state<string | null>(null);

	async function handleRoleChange(u: LocalUser, role: string) {
		if (role === u.role) return;
		const previous = u.role;
		u.role = role;
		roleSaving = u.id;
		usersError = '';
		try {
			await setUserRole(u.id, role);
		} catch (e) {
			u.role = previous;
			usersError = e instanceof Error ? e.message : String(e);
		} finally {
			roleSaving = null;
		}
	}

	// --- password management: an admin can type the new password
	// directly (the default) instead of always getting a random one
	// back -- generating one is still one click away for anyone who
	// wants that instead. ---
	let passwordTarget = $state<string | null>(null);
	let passwordInput = $state('');
	let passwordBusy = $state(false);
	let passwordShown = $state<{ userId: string; password: string } | null>(null);

	function togglePasswordPanel(id: string) {
		passwordTarget = passwordTarget === id ? null : id;
		passwordInput = '';
		passwordShown = null;
		usersError = '';
	}

	async function handleSetPassword(id: string) {
		if (passwordInput.length < 8) {
			usersError = 'Password must be at least 8 characters';
			return;
		}
		passwordBusy = true;
		usersError = '';
		try {
			await resetPassword(id, passwordInput);
			passwordTarget = null;
			passwordInput = '';
		} catch (e) {
			usersError = e instanceof Error ? e.message : String(e);
		} finally {
			passwordBusy = false;
		}
	}

	async function handleGeneratePassword(id: string) {
		passwordBusy = true;
		usersError = '';
		try {
			const { password } = await resetPassword(id);
			if (password) passwordShown = { userId: id, password };
		} catch (e) {
			usersError = e instanceof Error ? e.message : String(e);
		} finally {
			passwordBusy = false;
		}
	}

	// Rendered in the reader's display timezone like every other
	// timestamp -- a date is just a timestamp with the time cut off, and
	// near midnight the two zones genuinely disagree about which day it
	// was. See $lib/time.ts.
	function formatDate(iso: string): string {
		return formatDateInZone(iso, getTimezone());
	}
</script>

<main>
	<h1>Users</h1>

	{#if !checked}
		<p class="muted">Loading…</p>
	{:else if localSession === 'disabled'}
		<p class="note">
			This deployment doesn't have local user accounts enabled -- see the "Single sign-on" section on
			<a href="/settings">Settings</a> if it's using enterprise SSO instead.
		</p>
	{:else if localSession === null}
		<p class="note">Sign in to manage users.</p>
	{:else if localSession.role !== 'owner' && localSession.role !== 'admin'}
		<p class="note">
			Only an owner or admin can manage users. Signed in as {localSession.username} ({localSession.role}). To
			change your own password, use "Change password" in the sidebar.
		</p>
	{:else}
		<p class="subtitle">
			Local accounts for this deployment.
			{#if !isOwner}As an admin, you can manage viewer and editor accounts.{/if}
		</p>

		{#if usersError}<p class="error">{usersError}</p>{/if}

		{#if usersLoading}
			<p class="muted">Loading…</p>
		{:else}
			<table>
				<thead>
					<tr>
						<th>User</th>
						<th>Role</th>
						<th>Created</th>
						<th></th>
					</tr>
				</thead>
				<tbody>
					{#each users as u (u.id)}
						<tr>
							<td>
								<div class="user-cell">
									<span class="avatar" aria-hidden="true">{u.username.slice(0, 1).toUpperCase()}</span>
									<span class="username">{u.username}</span>
									{#if u.id === localSession.user_id}<span class="you">you</span>{/if}
								</div>
							</td>
							<td>
								<select
									class="role-select"
									value={u.role}
									disabled={roleSaving === u.id || !isOwner}
									title={isOwner ? '' : 'Only an owner can reassign roles'}
									onchange={(e) => handleRoleChange(u, e.currentTarget.value)}
									aria-label="Role for {u.username}"
								>
									{#each roleOptions as r (r)}
										<option value={r}>{r}</option>
									{/each}
								</select>
							</td>
							<td class="muted">{formatDate(u.created_at)}</td>
							<td class="actions">
								{#if u.id === localSession.user_id}
									<span class="muted self-note">Use "Change password" in the sidebar</span>
								{:else}
									<button
										type="button"
										onclick={() => togglePasswordPanel(u.id)}
										disabled={!canResetPassword(u)}
										title={canResetPassword(u) ? '' : "Only an owner can reset an owner's password"}
									>
										Change password
									</button>
								{/if}
								<button
									type="button"
									class="danger"
									onclick={() => handleDelete(u)}
									disabled={!canDeleteUser(u)}
									title={deleteDisabledReason(u)}
								>
									Delete
								</button>
							</td>
						</tr>
						{#if passwordTarget === u.id}
							<tr class="password-row">
								<td colspan="4">
									<div class="password-panel">
										{#if passwordShown?.userId === u.id}
											<p class="note">
												New password (shown once, save it now): <code>{passwordShown.password}</code>
											</p>
										{:else}
											<label for="pw-{u.id}">New password for {u.username}</label>
											<div class="password-row-inner">
												<input
													id="pw-{u.id}"
													type="text"
													placeholder="Type a new password (min. 8 characters)"
													bind:value={passwordInput}
													disabled={passwordBusy}
												/>
												<button
													type="button"
													disabled={passwordBusy}
													onclick={() => handleSetPassword(u.id)}
												>
													{passwordBusy ? 'Saving…' : 'Save password'}
												</button>
											</div>
											<button
												type="button"
												class="link"
												disabled={passwordBusy}
												onclick={() => handleGeneratePassword(u.id)}
											>
												Generate a random password instead
											</button>
										{/if}
									</div>
								</td>
							</tr>
						{/if}
					{/each}
				</tbody>
			</table>
		{/if}

		{#if showCreate}
			<form onsubmit={handleCreate} class="create-user">
				<div class="field">
					<label for="new-username">Username</label>
					<input id="new-username" type="text" bind:value={newUsername} disabled={creating} required />
				</div>
				<div class="field">
					<label for="new-password">Password</label>
					<input
						id="new-password"
						type="text"
						placeholder="Min. 8 characters"
						bind:value={newPassword}
						disabled={creating}
						required
					/>
				</div>
				<div class="field">
					<label for="new-role">Role</label>
					<select id="new-role" bind:value={newRole} disabled={creating}>
						{#each (isOwner ? roleOptions : adminCreatableRoles) as r (r)}
							<option value={r}>{r}</option>
						{/each}
					</select>
				</div>
				<div class="field-actions">
					<button type="submit" disabled={creating}>{creating ? 'Adding…' : 'Add user'}</button>
					<button type="button" class="link" onclick={() => (showCreate = false)} disabled={creating}
						>Cancel</button
					>
				</div>
			</form>
		{:else}
			<button type="button" class="add-user-btn" onclick={() => (showCreate = true)}>+ Add user</button>
		{/if}
	{/if}
</main>

<style>
	main {
		max-width: 44rem;
	}
	h1 {
		font-size: var(--text-xl);
		margin-bottom: var(--space-2);
	}
	.subtitle {
		color: var(--color-text-muted);
		font-size: var(--text-sm);
		margin-bottom: var(--space-5);
	}
	.muted {
		color: var(--color-text-muted);
	}
	.note {
		color: var(--color-text-muted);
		font-size: var(--text-sm);
	}
	.note a {
		color: var(--color-accent);
	}
	.error {
		color: var(--color-danger);
		font-size: var(--text-sm);
		margin-bottom: var(--space-3);
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
		padding: var(--space-3) var(--space-2);
		border-bottom: 1px solid var(--color-border);
		vertical-align: middle;
	}
	th {
		color: var(--color-text-muted);
		font-weight: var(--font-weight-medium);
		font-size: var(--text-xs);
		text-transform: uppercase;
		letter-spacing: 0.02em;
	}

	.user-cell {
		display: flex;
		align-items: center;
		gap: var(--space-2);
	}
	.avatar {
		display: flex;
		align-items: center;
		justify-content: center;
		width: 1.75rem;
		height: 1.75rem;
		flex: none;
		border-radius: 50%;
		background: color-mix(in srgb, var(--color-accent) 16%, var(--color-surface));
		color: var(--color-text);
		font-size: var(--text-xs);
		font-weight: var(--font-weight-medium);
	}
	.username {
		font-weight: var(--font-weight-medium);
		color: var(--color-text);
	}
	.you {
		font-size: var(--text-xs);
		color: var(--color-text-muted);
		border: 1px solid var(--color-border);
		border-radius: var(--radius-sm);
		padding: 0.05rem 0.35rem;
	}

	.role-select {
		font-family: var(--font-ui);
		font-size: var(--text-sm);
		color: var(--color-text);
		background: var(--color-surface);
		border: 1px solid var(--color-border);
		border-radius: var(--radius-sm);
		padding: var(--space-1) var(--space-2);
		text-transform: capitalize;
		cursor: pointer;
	}
	.role-select:disabled {
		cursor: default;
		opacity: 0.6;
	}

	.actions {
		display: flex;
		gap: var(--space-2);
		justify-content: flex-end;
		white-space: nowrap;
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
	.actions button:disabled {
		cursor: default;
		opacity: 0.5;
	}
	.self-note {
		font-size: var(--text-xs);
	}

	.password-row td {
		border-bottom: 1px solid var(--color-border);
		padding-top: 0;
	}
	.password-panel {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		padding: var(--space-3);
		background: var(--color-surface);
		border: 1px solid var(--color-border);
		border-radius: var(--radius-md);
	}
	.password-panel label {
		font-size: var(--text-xs);
		color: var(--color-text-muted);
	}
	.password-row-inner {
		display: flex;
		gap: var(--space-2);
	}
	.password-row-inner input {
		flex: 1;
		font-family: var(--font-mono);
		font-size: var(--text-sm);
		color: var(--color-text);
		background: var(--color-bg);
		border: 1px solid var(--color-border);
		border-radius: var(--radius-sm);
		padding: var(--space-2) var(--space-3);
	}
	.password-row-inner button {
		padding: var(--space-2) var(--space-4);
		font-family: var(--font-ui);
		font-size: var(--text-sm);
		font-weight: var(--font-weight-medium);
		color: var(--color-bg);
		background: var(--color-accent);
		border: none;
		border-radius: var(--radius-sm);
		cursor: pointer;
		white-space: nowrap;
	}
	.password-row-inner button:disabled {
		cursor: default;
		opacity: 0.6;
	}
	code {
		font-family: var(--font-mono);
		background: var(--color-bg);
		border: 1px solid var(--color-border);
		border-radius: 3px;
		padding: 0.1rem 0.4rem;
	}

	.link {
		align-self: flex-start;
		background: none;
		border: none;
		padding: 0;
		color: var(--color-accent);
		font-family: var(--font-ui);
		font-size: var(--text-xs);
		cursor: pointer;
	}
	.link:disabled {
		cursor: default;
		opacity: 0.6;
	}

	.add-user-btn {
		padding: var(--space-2) var(--space-4);
		font-family: var(--font-ui);
		font-size: var(--text-sm);
		font-weight: var(--font-weight-medium);
		color: var(--color-text);
		background: var(--color-surface);
		border: 1px solid var(--color-border);
		border-radius: var(--radius-sm);
		cursor: pointer;
	}
	.add-user-btn:hover {
		border-color: var(--color-border-strong);
	}

	.create-user {
		display: flex;
		flex-wrap: wrap;
		align-items: flex-end;
		gap: var(--space-3);
		padding: var(--space-4);
		background: var(--color-surface);
		border: 1px solid var(--color-border);
		border-radius: var(--radius-md);
	}
	.field {
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
	}
	.field label {
		font-size: var(--text-xs);
		color: var(--color-text-muted);
	}
	.field input,
	.field select {
		font-family: var(--font-ui);
		font-size: var(--text-sm);
		color: var(--color-text);
		background: var(--color-bg);
		border: 1px solid var(--color-border);
		border-radius: var(--radius-sm);
		padding: var(--space-2) var(--space-3);
	}
	.field-actions {
		display: flex;
		align-items: center;
		gap: var(--space-3);
	}
	.field-actions button[type='submit'] {
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
	.field-actions button[type='submit']:disabled {
		cursor: default;
		opacity: 0.6;
	}
</style>
