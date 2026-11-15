<script lang="ts">
	import { page } from '$app/state';
	import Icon from './Icon.svelte';
	import Mark from './Mark.svelte';
	import { post, type Provider } from './api';
	import { live } from './live.svelte';
	import { passkeysSupported, signInWithPasskey } from './passkey';
	let { dev, demo = false, providers }: { dev: boolean; demo?: boolean; providers: Provider[] } = $props();
	let busy = $state(false);
	let error = $state('');

	// Why the last sign-in didn't work, from obs-beacond's redirect (?signin=…).
	const MESSAGES: Record<string, string> = {
		refused: 'That account isn’t allowed into Cairn OBS. Nothing about it was kept.',
		expired: 'That sign-in took too long or came from somewhere else. Please try again.',
		cancelled: 'Sign-in was cancelled.',
		unreachable: 'The sign-in service can’t be reached right now.',
		failed: 'Sign-in didn’t work. Please try again.'
	};
	const reason = $derived(MESSAGES[page.url.searchParams.get('signin') ?? ''] ?? '');
	const back = $derived(page.url.pathname === '/' ? '' : '?return=' + encodeURIComponent(page.url.pathname + page.url.search));

	const redirects = $derived(providers.filter((p) => p.kind !== 'passkey'));
	const local = $derived(providers.find((p) => p.kind === 'passkey'));
	const passkey = $derived(local && local.passkeys !== false ? local : undefined);
	const passwords = $derived(!!local?.passwords);
	let pw = $state({ email: '', password: '', code: '' });
	async function withPassword(e: SubmitEvent) {
		e.preventDefault();
		busy = true;
		error = '';
		try {
			const r = await fetch('/auth/password', { method: 'POST', credentials: 'same-origin', headers: { 'X-Beacon': '1', 'Content-Type': 'application/json' }, body: JSON.stringify(pw) });
			if (!r.ok) throw new Error((await r.json().catch(() => ({}))).error ?? `${r.status} ${r.statusText}`);
			pw = { email: '', password: '', code: '' };
			await live.checkSession();
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
			pw.code = '';
		} finally {
			busy = false;
		}
	}
	async function withPasskey() {
		busy = true;
		error = '';
		try {
			await signInWithPasskey();
			await live.checkSession();
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		} finally {
			busy = false;
		}
	}

	async function demoSignin() {
		busy = true;
		error = '';
		try {
			await post('/auth/demo');
			await live.checkSession();
		} catch (e) {
			error = String(e);
		} finally {
			busy = false;
		}
	}

	async function devSignin() {
		busy = true;
		error = '';
		try {
			await post('/auth/dev');
			await live.checkSession();
		} catch (e) {
			error = String(e);
		} finally {
			busy = false;
		}
	}
</script>

<div class="signin">
	<div class="gridbg"></div>
	<div class="box">
		<div class="brand"><Mark size={34} /><b>Cairn OBS</b></div>
		<p class="lead">Self-hosted monitoring</p>
		{#if reason}<p class="reason" role="alert">{reason}</p>{/if}
		{#if demo}
			<p class="demo">This is the demo: a simulated enterprise of hosts, clusters, VMs, arrays and IBM i, failing in the ways real ones do. Look at anything, setup included; nothing you do changes it.</p>
			<button class="btn pri wide" onclick={demoSignin} disabled={busy}><Icon name="eye" /> Look around</button>
			<p class="fine"><a href="https://cairnobs.org/">What Cairn OBS is</a></p>
		{/if}
		{#each redirects as p (p.id)}
			<a class="btn pri wide" href="/auth/{p.id}/login{back}" data-sveltekit-reload data-sveltekit-preload-data="off"><Icon name="lock" /> {p.label}</a>
			{#if p.note}<p class="fine">{p.note}</p>{/if}
		{/each}
		{#if passkey}
			<button class="btn wide" onclick={withPasskey} disabled={busy || !passkeysSupported()}><Icon name="key" /> {passkey.label}</button>
			<p class="fine">{passkeysSupported() ? 'For accounts an admin added to Cairn OBS itself.' : 'This browser can’t use passkeys.'}</p>
		{/if}
		{#if passwords}
			{#if passkey}<div class="or"><span>or</span></div>{/if}
			<form class="pw" onsubmit={withPassword}>
				<label>Email <input type="email" bind:value={pw.email} autocomplete="username" required /></label>
				<label>Password <input type="password" bind:value={pw.password} autocomplete="current-password" required /></label>
				<label>Code from your authenticator app <input bind:value={pw.code} inputmode="numeric" autocomplete="one-time-code" pattern={'[0-9 ]{6,7}'} maxlength="7" required /></label>
				<button class="btn {passkey ? '' : 'pri'} wide" type="submit" disabled={busy}><Icon name="lock" /> Sign in</button>
			</form>
		{/if}
		{#if !providers.length && !demo}<p class="fine">No sign-in provider is configured.</p>{/if}
		{#if dev}
			<div class="dev">
				<p>This obs-beacond runs with <b>development sign-in</b>, which only works on a loopback address.</p>
				<button class="btn wide" onclick={devSignin} disabled={busy}>Sign in for local development</button>
			</div>
		{/if}
		{#if error}<p class="err">{error}</p>{/if}
	</div>
</div>

<style>
	.signin { min-height: 100vh; display: grid; place-items: center; padding: 16px; position: relative; overflow: hidden; }
	.gridbg { position: absolute; inset: 0; opacity: .5; background-image: linear-gradient(var(--border) 1px, transparent 1px), linear-gradient(90deg, var(--border) 1px, transparent 1px); background-size: 24px 24px; mask-image: radial-gradient(circle at 50% 40%, #000, transparent 70%); }
	.box { position: relative; width: min(400px, 100%); border: 1px solid var(--border); border-radius: 8px; background: var(--card); box-shadow: var(--shadow); padding: 32px 28px 24px; display: grid; gap: 10px; }
	.brand { display: flex; align-items: center; gap: 10px; justify-content: center; }
	.brand b { font-size: 30px; font-weight: 700; letter-spacing: -.03em; }
	.lead { text-align: center; color: var(--muted-fg); margin: 0 0 14px; }
	.wide { width: 100%; justify-content: center; height: 42px; }
	.fine { font-size: 12px; color: var(--muted-fg); text-align: center; line-height: 1.5; margin: 4px 0 0; }
	.reason { margin: 0 0 6px; padding: 10px 12px; border-radius: var(--radius); border: 1px solid color-mix(in oklab, var(--critical) 45%, transparent); background: color-mix(in oklab, var(--critical) 8%, var(--card)); font-size: 13px; }
	.dev { margin-top: 10px; padding-top: 16px; border-top: 1px solid var(--border); }
	.dev p { font-size: 12px; color: var(--muted-fg); margin: 0 0 10px; }
	.err { color: var(--critical); font-size: 13px; }
	.demo { font-size: 13.5px; line-height: 1.55; text-align: center; margin: 0 0 6px; }
	.fine a { color: inherit; }
	.or { display: flex; align-items: center; gap: 10px; color: var(--muted-fg); font-size: 12px; margin: 6px 0; }
	.or::before, .or::after { content: ''; flex: 1; border-top: 1px solid var(--border); }
	.pw { display: grid; gap: 10px; }
	.pw label { display: grid; gap: 5px; font-size: 12.5px; color: var(--muted-fg); }
	.pw input { height: 38px; padding: 0 10px; border-radius: var(--radius); border: 1px solid var(--input); background: var(--background); color: var(--foreground); font: inherit; }
</style>
