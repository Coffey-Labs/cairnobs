<script lang="ts">
	import { onMount } from 'svelte';
	import Icon from '$lib/Icon.svelte';
	import Mark from '$lib/Mark.svelte';
	import { deviceName, passkeysSupported, registerPasskey, setupInfo, type SetupInfo } from '$lib/passkey';

	// The token is in the fragment (#…), so it never reaches a server log.
	// A first-admin link (first-…) comes from obs-beacond's own log.
	let token = $state('');
	let first = $state(false);
	let who = $state<SetupInfo | null>(null);
	let name = $state('');
	let error = $state('');
	let busy = $state(false);
	let done = $state(false);
	let method = $state<'passkey' | 'password'>('passkey');
	let admin = $state({ name: '', email: '' });
	let pw = $state({ password: '', again: '', code: '' });
	let totp = $state<{ secret: string; uri: string } | null>(null);

	async function call(url: string, body: unknown) {
		const r = await fetch(url, { method: 'POST', credentials: 'same-origin', headers: { 'X-Beacon': '1', 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
		const data = await r.json().catch(() => ({}));
		if (!r.ok) throw new Error(data.error ?? `${r.status} ${r.statusText}`);
		return data;
	}

	async function load() {
		who = await setupInfo(token);
		const canPasskey = who.passkeys !== false && passkeysSupported();
		method = canPasskey || !who.passwords ? 'passkey' : 'password';
		if (method === 'password') await startTOTP();
	}

	// Once: the link leaves the address bar straight away.
	onMount(() => {
		token = location.hash.slice(1);
		history.replaceState(null, '', '/setup'); // keep the link out of the history and the address bar
		name = deviceName();
		if (!token) {
			error = 'This page needs the setup link an admin sent you.';
			return;
		}
		if (token.startsWith('first-')) {
			call('/auth/first', { token, check: true }).then(() => (first = true)).catch((e) => (error = e.message));
			return;
		}
		load().catch((e) => (error = e.message));
	});

	async function makeAdmin(e: SubmitEvent) {
		e.preventDefault();
		busy = true;
		error = '';
		try {
			const r = await call('/auth/first', { token, ...admin });
			token = r.setup;
			first = false;
			await load();
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		} finally {
			busy = false;
		}
	}

	async function startTOTP() {
		method = 'password';
		error = '';
		if (!totp) totp = await call('/auth/setup/totp', { token });
	}

	async function create() {
		busy = true;
		error = '';
		try {
			await registerPasskey(token, name.trim() || 'Passkey');
			finish();
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		} finally {
			busy = false;
		}
	}

	async function setPassword(e: SubmitEvent) {
		e.preventDefault();
		if (pw.password !== pw.again) {
			error = 'The two passwords differ.';
			return;
		}
		busy = true;
		error = '';
		try {
			await call('/auth/setup/password', { token, password: pw.password, code: pw.code });
			finish();
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
			pw.code = '';
		} finally {
			busy = false;
		}
	}

	function finish() {
		done = true;
		setTimeout(() => (location.href = '/'), 1200);
	}
	const grouped = (s: string) => s.replace(/(.{4})/g, '$1 ').trim();
</script>

<svelte:head><title>Cairn OBS · Set up sign-in</title></svelte:head>

<div class="signin">
	<div class="gridbg"></div>
	<div class="box">
		<div class="brand"><Mark size={34} /><b>Cairn OBS</b></div>
		{#if done}
			<p class="lead">You're set up. Taking you in…</p>
		{:else if first}
			<p class="lead">Welcome. Make the first admin account.</p>
			<p class="fine">This link came from obs-beacond's log and works once. Whoever finishes it is an admin and can add everyone else.</p>
			<form class="form" onsubmit={makeAdmin}>
				<label class="field">Your name <input bind:value={admin.name} required maxlength="80" autocomplete="name" /></label>
				<label class="field">Email <input type="email" bind:value={admin.email} required autocomplete="email" /></label>
				<button class="btn pri wide" type="submit" disabled={busy}>Continue</button>
			</form>
		{:else if who}
			{#if who.emergency}
				<p class="lead">Hello {who.name}. Set up the passkey for Cairn OBS's <b>emergency account</b>.</p>
				<p class="warn">It will work only through this SSH tunnel, never on the public site. Every time it's used, every alert channel is told. Keep it on a device you'll have when everything else is down, such as your phone.</p>
			{:else}
				<p class="lead">Hello {who.name}. Set up how you sign in to Cairn OBS as {who.role === 'admin' ? 'an admin' : who.role === 'operator' ? 'an operator' : 'a viewer'}.</p>
			{/if}

			{#if method === 'passkey'}
				{#if who.passkeys === false}
					<p class="err">Passkeys need Cairn OBS on HTTPS, and this page isn't. Ask an admin to set alerts.public_url to its https:// address.</p>
				{:else}
					<p class="fine">A passkey lives on this device (or your phone or security key) and needs your fingerprint, face or PIN. There's no password to remember or leak.</p>
					<label class="field">Name it <input bind:value={name} maxlength="60" autocomplete="off" /></label>
					<button class="btn pri wide" onclick={create} disabled={busy || !passkeysSupported()}><Icon name="key" /> {busy ? 'Waiting for your device…' : 'Create passkey'}</button>
					{#if !passkeysSupported()}<p class="err">This browser can’t create passkeys. Try a current Chrome, Safari, Edge or Firefox.</p>{/if}
				{/if}
				{#if who.passwords}
					<button class="link" onclick={() => startTOTP().catch((e) => (error = e.message))}>Use a password and an authenticator app instead</button>
				{/if}
			{:else}
				{#if !who.secure}
					<p class="warn">This page isn't HTTPS, so your password crosses the network unencrypted. That's fine for a first look on a network you trust; set up HTTPS before real use.</p>
				{/if}
				<p class="fine">You'll sign in with a password <b>and</b> a six-digit code from an authenticator app (Google Authenticator, 1Password, Aegis, …), never the password alone.</p>
				{#if totp}
					<div class="totp">
						<span class="step">1</span>
						<div>
							<p>Add Cairn OBS to your authenticator app with this key{#if /Android|iPhone|iPad/.test(navigator.userAgent)}, or <a href={totp.uri}>open it in the app</a>{/if}:</p>
							<p class="key mono">{grouped(totp.secret)}</p>
							<p class="fine">Time-based, six digits, every 30 seconds.</p>
						</div>
					</div>
				{/if}
				<form class="form" onsubmit={setPassword}>
					<div class="totp"><span class="step">2</span><div class="form">
						<label class="field">Password (12 characters or more) <input type="password" bind:value={pw.password} minlength="12" maxlength="256" required autocomplete="new-password" /></label>
						<label class="field">Again <input type="password" bind:value={pw.again} required autocomplete="new-password" /></label>
						<label class="field">The code the app shows now <input bind:value={pw.code} inputmode="numeric" pattern={'[0-9 ]{6,7}'} maxlength="7" required autocomplete="one-time-code" /></label>
					</div></div>
					<button class="btn pri wide" type="submit" disabled={busy || !totp}><Icon name="lock" /> Set password and sign in</button>
				</form>
				{#if who.passkeys !== false && passkeysSupported()}
					<button class="link" onclick={() => (method = 'passkey')}>Use a passkey instead</button>
				{/if}
			{/if}
			<p class="fine">Signed in as <span class="mono">{who.email}</span> once it's done. The link stops working after this.</p>
		{:else if !error}
			<p class="lead">Checking your link…</p>
		{/if}
		{#if error}<p class="err" role="alert">{error}</p>{/if}
	</div>
</div>

<style>
	.signin { min-height: 100vh; display: grid; place-items: center; padding: 16px; position: relative; overflow: hidden; background: var(--content); }
	.gridbg { position: absolute; inset: 0; opacity: .5; background-image: linear-gradient(var(--border) 1px, transparent 1px), linear-gradient(90deg, var(--border) 1px, transparent 1px); background-size: 24px 24px; mask-image: radial-gradient(circle at 50% 40%, #000, transparent 70%); }
	.box { position: relative; width: min(440px, 100%); background: var(--card); border: 1px solid var(--border); border-radius: var(--radius); padding: 28px; box-shadow: var(--shadow); display: grid; gap: 14px; }
	.brand { display: flex; align-items: center; gap: 10px; font-size: 22px; }
	.lead { margin: 0; font-size: 15px; }
	.fine { margin: 0; font-size: 12.5px; color: var(--muted-fg); }
	.err { margin: 0; font-size: 13px; color: var(--critical); }
	.warn { margin: 0; font-size: 13px; border-left: 3px solid var(--critical); padding-left: 10px; }
	.wide { width: 100%; justify-content: center; }
	.form { display: grid; gap: 10px; }
	.field { display: grid; gap: 6px; font-size: 13px; color: var(--muted-fg); }
	.field input { height: 36px; padding: 0 10px; border-radius: var(--radius); border: 1px solid var(--input); background: var(--background); color: var(--foreground); font: inherit; }
	.link { background: none; border: 0; padding: 0; color: var(--primary); font: inherit; font-size: 13px; cursor: pointer; justify-self: center; text-decoration: underline; }
	.totp { display: grid; grid-template-columns: 24px 1fr; gap: 10px; align-items: start; }
	.totp p { margin: 0 0 6px; font-size: 13px; }
	.step { width: 22px; height: 22px; border-radius: 50%; display: grid; place-items: center; font: 600 12px var(--mono); border: 1px solid var(--border); color: var(--muted-fg); }
	.key { font-size: 15px; letter-spacing: .04em; padding: 8px 10px; border: 1px dashed var(--border); border-radius: var(--radius); user-select: all; word-spacing: .2em; }
</style>
