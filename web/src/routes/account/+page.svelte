<script lang="ts">
	import { localAuthEnabled, getLocalSession, changeOwnPassword, type LocalSession } from '$lib/api';

	// Deliberately no role gating anywhere on this page -- changing your
	// own password is available to every role (viewer through owner),
	// unlike everything on /users which is owner/admin only. See
	// api/localauth/handler.go's handleChangeOwnPassword doc comment.
	let localSession = $state<LocalSession | 'disabled' | null>(null);
	let checked = $state(false);
	$effect(() => {
		if (!localAuthEnabled) {
			checked = true;
			return;
		}
		getLocalSession().then((s) => {
			localSession = s;
			checked = true;
		});
	});

	let currentPassword = $state('');
	let newPassword = $state('');
	let confirmPassword = $state('');
	let saving = $state(false);
	let error = $state('');
	let done = $state(false);

	async function submit(e: SubmitEvent) {
		e.preventDefault();
		error = '';
		if (newPassword.length < 8) {
			error = 'New password must be at least 8 characters.';
			return;
		}
		if (newPassword !== confirmPassword) {
			error = 'New password and confirmation do not match.';
			return;
		}
		saving = true;
		try {
			await changeOwnPassword(currentPassword, newPassword);
			done = true;
			// The server just revoked this session as part of the change
			// (see handleChangeOwnPassword) -- the cookie is already dead,
			// so send the user to sign back in rather than leaving them on
			// a page that looks live but whose next request will 401.
			setTimeout(() => (window.location.href = '/login'), 1500);
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		} finally {
			saving = false;
		}
	}
</script>

<main>
	<h1>Change password</h1>

	{#if !checked}
		<p class="muted">Loading…</p>
	{:else if localSession === 'disabled'}
		<p class="note">This deployment doesn't have local accounts enabled.</p>
	{:else if localSession === null}
		<p class="note">Sign in to change your password.</p>
	{:else if done}
		<p class="note">Password changed. Signing you out so you can sign back in…</p>
	{:else}
		<p class="subtitle">Signed in as {localSession.username} ({localSession.role}).</p>

		<form onsubmit={submit} class="password-form">
			<div class="field">
				<label for="current-password">Current password</label>
				<input
					id="current-password"
					type="password"
					bind:value={currentPassword}
					disabled={saving}
					autocomplete="current-password"
					required
				/>
			</div>
			<div class="field">
				<label for="new-password">New password</label>
				<input
					id="new-password"
					type="password"
					placeholder="Min. 8 characters"
					bind:value={newPassword}
					disabled={saving}
					autocomplete="new-password"
					required
				/>
			</div>
			<div class="field">
				<label for="confirm-password">Confirm new password</label>
				<input
					id="confirm-password"
					type="password"
					bind:value={confirmPassword}
					disabled={saving}
					autocomplete="new-password"
					required
				/>
			</div>

			{#if error}<p class="error">{error}</p>{/if}

			<button type="submit" disabled={saving}>{saving ? 'Saving…' : 'Change password'}</button>
		</form>
		<p class="note">Changing your password signs you out everywhere -- you'll need to sign back in.</p>
	{/if}
</main>

<style>
	main {
		max-width: 26rem;
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
		margin-top: var(--space-3);
	}
	.error {
		color: var(--color-danger);
		font-size: var(--text-sm);
	}
	.password-form {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
	}
	.field {
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
	}
	.field label {
		font-size: var(--text-sm);
		color: var(--color-text-muted);
	}
	.field input {
		font-family: var(--font-ui);
		font-size: var(--text-sm);
		color: var(--color-text);
		background: var(--color-surface);
		border: 1px solid var(--color-border);
		border-radius: var(--radius-sm);
		padding: var(--space-2) var(--space-3);
	}
	.password-form button {
		align-self: flex-start;
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
	.password-form button:disabled {
		cursor: default;
		opacity: 0.6;
	}
</style>
