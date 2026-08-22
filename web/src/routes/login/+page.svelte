<script lang="ts">
	import { page } from '$app/state';
	import { login, demoUsername, demoPassword } from '$lib/api';

	// A public demo fills its own credentials in (see api.ts's
	// demoUsername). Both or neither: prefilling one field alone would
	// just look like a bug. Still an ordinary editable form -- the values
	// are a starting point, not a lock, so the demo's admin account can
	// still be typed in over them.
	const prefilled = Boolean(demoUsername && demoPassword);

	let username = $state(prefilled ? (demoUsername as string) : '');
	let password = $state(prefilled ? (demoPassword as string) : '');
	let error = $state('');
	let submitting = $state(false);

	async function submit(e: SubmitEvent) {
		e.preventDefault();
		if (submitting) return;
		submitting = true;
		error = '';
		try {
			await login(username, password);
			// Full navigation, not SvelteKit's router -- same reasoning
			// select-tenant/+page.svelte's choose() gives for the tenant
			// picker: reloading picks up the session cookie login() just
			// set, which client-side routing wouldn't need to know about
			// but a fresh page load makes unambiguous.
			const next = page.url.searchParams.get('next') || '/';
			window.location.href = next;
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
			submitting = false;
		}
	}
</script>

<main>
	<h1>Sign in</h1>
	{#if prefilled}
		<p class="demo-note">
			Public demo &mdash; the read-only <code>{demoUsername}</code> account is already filled in.
			Just sign in.
		</p>
	{/if}
	<form onsubmit={submit}>
		{#if error}
			<p class="error">{error}</p>
		{/if}
		<label>
			<span>Username</span>
			<input type="text" autocomplete="username" bind:value={username} disabled={submitting} required />
		</label>
		<label>
			<span>Password</span>
			<input
				type="password"
				autocomplete="current-password"
				bind:value={password}
				disabled={submitting}
				required
			/>
		</label>
		<button type="submit" disabled={submitting}>{submitting ? 'Signing in…' : 'Sign in'}</button>
	</form>
</main>

<style>
	main {
		max-width: 22rem;
		margin: var(--space-8) auto;
	}
	h1 {
		font-size: var(--text-lg);
		margin-bottom: var(--space-5);
	}
	form {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
	}
	label {
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
		font-size: var(--text-sm);
		color: var(--color-text-muted);
	}
	input {
		font-family: var(--font-ui);
		font-size: var(--text-base);
		color: var(--color-text);
		background: var(--color-surface);
		border: 1px solid var(--color-border);
		border-radius: var(--radius-sm);
		padding: var(--space-2) var(--space-3);
	}
	input:focus {
		outline: none;
		border-color: var(--color-accent);
	}
	button {
		margin-top: var(--space-2);
		padding: var(--space-2) var(--space-4);
		font-family: var(--font-ui);
		font-size: var(--text-base);
		font-weight: var(--font-weight-medium);
		color: var(--color-bg);
		background: var(--color-accent);
		border: none;
		border-radius: var(--radius-sm);
		cursor: pointer;
	}
	button:disabled {
		cursor: default;
		opacity: 0.6;
	}
	.error {
		color: var(--color-danger);
		font-size: var(--text-sm);
	}
	.demo-note {
		margin-bottom: var(--space-4);
		padding: var(--space-3);
		border: 1px solid var(--color-border);
		border-radius: var(--radius-sm);
		background: var(--color-surface);
		color: var(--color-text-muted);
		font-size: var(--text-sm);
		line-height: 1.5;
	}
	.demo-note code {
		font-family: var(--font-mono);
		color: var(--color-text);
	}
</style>
