<script lang="ts">
	import Icon from '$lib/Icon.svelte';
	import Panel from '$lib/Panel.svelte';
	import Status from '$lib/Status.svelte';
	import { get, SignedOut } from '$lib/api';
	import { live } from '$lib/live.svelte';
	import { ago, hmz, utc } from '$lib/format';

	type Passkey = { id: string; name: string; created: string; last_used?: string };
	type User = { id: string; name: string; email: string; role: string; created: string; disabled: boolean; last_sign_in?: string; passkeys: Passkey[]; pending_setup: boolean; emergency: boolean; password: boolean };

	let users = $state<User[]>([]);
	let configured = $state(true);
	let door = $state<{ origin: string; listen: string }>({ origin: '', listen: '' });
	let passwords = $state(false);
	let error = $state('');
	let form = $state({ name: '', email: '', role: 'viewer', emergency: false });
	let busy = $state(false);
	let link = $state<{ for: string; url: string; emergency: boolean } | null>(null);
	let copied = $state(false);

	async function load() {
		try {
			const r = await get<{ configured: boolean; users: User[]; door?: string; door_listen?: string; passwords?: boolean }>('/api/v1/users');
			passwords = !!r.passwords;
			configured = r.configured;
			door = { origin: r.door ?? '', listen: r.door_listen ?? '' };
			users = r.users;
			error = '';
		} catch (e) {
			if (e instanceof SignedOut) void live.checkSession();
			else error = String(e);
		}
	}
	$effect(() => {
		void load();
	});

	async function call(method: string, url: string, body?: unknown) {
		const r = await fetch(url, { method, credentials: 'same-origin', headers: { 'X-Beacon': '1', 'Content-Type': 'application/json' }, body: body ? JSON.stringify(body) : undefined });
		const text = await r.text();
		const data = text ? JSON.parse(text) : null;
		if (!r.ok) throw new Error(data?.error ?? r.statusText);
		return data;
	}
	async function act(fn: () => Promise<void>) {
		busy = true;
		error = '';
		try {
			await fn();
			await load();
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		} finally {
			busy = false;
		}
	}
	const add = () => act(async () => {
		const r = await call('POST', '/api/v1/users', form);
		link = { for: r.user.name, url: r.setup_link, emergency: r.user.emergency };
		copied = false;
		form = { name: '', email: '', role: 'viewer', emergency: false };
	});
	const newLink = (u: User) => act(async () => {
		const r = await call('POST', `/api/v1/users/${u.id}/setup-link`);
		link = { for: u.name, url: r.setup_link, emergency: u.emergency };
		copied = false;
	});
	const setRole = (u: User, role: string) => act(() => call('POST', `/api/v1/users/${u.id}`, { role }));
	const setDisabled = (u: User, disabled: boolean) => act(() => call('POST', `/api/v1/users/${u.id}`, { disabled }));
	const remove = (u: User) => act(() => call('DELETE', `/api/v1/users/${u.id}`));
	const removeKey = (u: User, p: Passkey) => act(() => call('DELETE', `/api/v1/users/${u.id}/passkeys/${encodeURIComponent(p.id)}`));
	const removePassword = (u: User) => act(() => call('DELETE', `/api/v1/users/${u.id}/password`));
	let confirming = $state<string | null>(null);

	async function copy() {
		if (!link) return;
		try {
			await navigator.clipboard.writeText(link.url);
			copied = true;
		} catch {
			copied = false;
		}
	}
	const stateOf = (u: User) => (u.disabled ? (['off', 'Disabled'] as const) : u.passkeys.length || u.password ? (['good', 'Active'] as const) : u.pending_setup ? (['waiting', 'Waiting for setup'] as const) : (['warning', 'Can’t sign in'] as const));
</script>

<svelte:head><title>Cairn OBS · Users</title></svelte:head>

<div class="phead">
	<span class="tile"><Icon name="users" /></span>
	<div>
		<h1>Users</h1>
		<p>Accounts Cairn OBS keeps itself. They sign in with a passkey{#if passwords}, or a password together with an authenticator app's code (never a password alone){:else}; there are no passwords{/if}. Admins change everything. Operators see everything, log lines and setup included, and handle alerts. Viewers see what's watched, without log lines, setup or the audit log.</p>
	</div>
</div>

{#if error}<p class="foot-note" style="color:var(--critical)">{error}</p>{/if}

{#if !configured}
	<Panel title="Not set up">
		<p class="sub">Add a <span class="mono">local</span> provider to <span class="mono">signin.providers</span> in obs-beacond.yaml, with <span class="mono">origin</span> set to Cairn OBS's own address. See docs/formats.md.</p>
	</Panel>
{:else}
	{#if link}
		<div class="linkbox" role="status">
			<b>Setup link for {link.for}</b>
			{#if link.emergency}
				<p class="sub">An emergency account's link opens only through the emergency door. On the computer that will hold the passkey, open a tunnel first, then the link: <span class="mono">ssh -L {door.listen.split(':').at(-1)}:{door.listen} &lt;obs-beacond's host&gt;</span>. It works once, for 24 hours.</p>
			{:else}
				<p class="sub">Send this to them yourself. It works once, for 24 hours, and opens a page where they set up how they sign in.</p>
			{/if}
			<div class="row"><input class="mono" readonly value={link.url} onfocus={(e) => e.currentTarget.select()} /><button class="btn sm" onclick={copy}>{copied ? 'Copied' : 'Copy'}</button><button class="btn sm" onclick={() => (link = null)}>Done</button></div>
		</div>
	{/if}

	<Panel title="Accounts" aside="{users.length} {users.length === 1 ? 'account' : 'accounts'}" flush>
		<div class="tbl-wrap flat">
			<table class="tbl">
				<thead><tr><th>Person</th><th>Role</th><th>State</th><th>Sign-in</th><th>Last sign-in</th><th></th></tr></thead>
				<tbody>
					{#each users as u (u.id)}
						{@const [s, w] = stateOf(u)}
						<tr>
							<td><div class="nm">{u.name}{#if u.emergency}<span class="badge">Emergency</span>{/if}</div><div class="sub mono">{u.email}</div></td>
							<td>
								{#if u.emergency}
									<span class="sub">Admin, emergency door only</span>
								{:else}
									<select value={u.role} disabled={busy || !live.admin} onchange={(e) => setRole(u, e.currentTarget.value)} aria-label="Role for {u.name}">
										<option value="viewer">Viewer</option>
										<option value="operator">Operator</option>
										<option value="admin">Admin</option>
									</select>
								{/if}
							</td>
							<td><Status {s} word={w} /></td>
							<td>
								{#each u.passkeys as p (p.id)}
									<div class="pk"><Icon name="key" size={13} /><span title="Added {utc(p.created)}">{p.name}</span>
										<span class="sub">{p.last_used ? 'used ' + ago(p.last_used) : ''}</span>
										<button class="x" title="Remove this passkey" aria-label="Remove passkey {p.name}" disabled={busy} hidden={!live.admin} onclick={() => removeKey(u, p)}>×</button></div>
								{:else}
									{#if !u.password}<span class="sub">None yet</span>{/if}
								{/each}
								{#if u.password}
									<div class="pk"><Icon name="lock" size={13} /><span>Password + code</span>
										<button class="x" title="Remove the password and authenticator" aria-label="Remove password for {u.name}" disabled={busy} hidden={!live.admin} onclick={() => removePassword(u)}>×</button></div>
								{/if}
							</td>
							<td class="sub">{u.last_sign_in ? `${hmz(u.last_sign_in)} · ${ago(u.last_sign_in)}` : 'Never'}</td>
							<td class="acts">
								{#if live.admin}
								<button class="btn sm" disabled={busy} onclick={() => newLink(u)}>{u.passkeys.length || u.password ? 'Link for another sign-in' : 'New setup link'}</button>
								<button class="btn sm" disabled={busy} onclick={() => setDisabled(u, !u.disabled)}>{u.disabled ? 'Enable' : 'Disable'}</button>
								{#if confirming === u.id}
									<button class="btn sm danger" disabled={busy} onclick={() => remove(u)}>Really remove</button>
									<button class="btn sm" onclick={() => (confirming = null)}>Keep</button>
								{:else}
									<button class="btn sm" disabled={busy} onclick={() => (confirming = u.id)}>Remove</button>
								{/if}
								{/if}
							</td>
						</tr>
					{:else}
						<tr><td colspan="6" class="empty">No local accounts yet. People who sign in with single sign-on don't need one.</td></tr>
					{/each}
				</tbody>
			</table>
		</div>
	</Panel>

	{#if live.admin}
	<div class="mt">
		<Panel title="Add someone">
			<form class="add" onsubmit={(e) => { e.preventDefault(); void add(); }}>
				<label>Name <input bind:value={form.name} required maxlength="80" autocomplete="off" /></label>
				<label>Email <input type="email" bind:value={form.email} required autocomplete="off" /></label>
				<label>Role
					<select bind:value={form.role} disabled={form.emergency}>
						<option value="viewer">Viewer: sees what's watched; no log lines, no setup, changes nothing</option>
						<option value="operator">Operator: sees everything, log lines and setup included, and handles alerts</option>
						<option value="admin">Admin: everything, including changing the setup and this page</option>
					</select>
				</label>
				<button class="btn pri" type="submit" disabled={busy}><Icon name="users" /> Add and make a setup link</button>
				{#if door.origin}
					<label class="check"><input type="checkbox" bind:checked={form.emergency} /> Emergency account: an admin who signs in only through the emergency door (an SSH tunnel to Cairn OBS's host), for when single sign-on is down. Every use alerts every channel.</label>
				{/if}
			</form>
		</Panel>
	</div>
	{/if}
	<p class="foot-note">Removing or disabling an account ends its sessions at once. Everything here goes in the audit log.</p>
{/if}

<style>
	.flat { border: 0; border-radius: 0; }
	.nm { font-weight: 600; }
	.pk { display: flex; align-items: center; gap: 6px; font-size: 13px; }
	.x { border: 0; background: none; color: var(--muted-fg); cursor: pointer; font-size: 15px; line-height: 1; padding: 0 4px; }
	.x:hover { color: var(--critical); }
	.acts { white-space: nowrap; text-align: right; }
	.acts .btn { margin-left: 4px; }
	.sm { height: 30px; padding: 0 10px; font-size: 12.5px; }
	.danger { border-color: var(--critical); color: var(--critical); }
	select, input { height: 34px; padding: 0 10px; border-radius: var(--radius); border: 1px solid var(--input); background: var(--background); color: var(--foreground); font: inherit; font-size: 13px; }
	.add { display: grid; grid-template-columns: minmax(0, 1fr) minmax(0, 1fr) minmax(0, 1.4fr) auto; gap: 12px; align-items: end; }
	.add select { width: 100%; min-width: 0; }
	.add label { display: grid; gap: 6px; font-size: 12.5px; color: var(--muted-fg); }
	.add label.check { grid-column: 1 / -1; display: flex; align-items: center; gap: 8px; }
	.add label.check input { height: auto; }
	.badge { margin-left: 8px; padding: 1px 7px; border-radius: 10px; font: 600 10.5px var(--mono); letter-spacing: .06em; text-transform: uppercase; color: var(--critical); border: 1px solid var(--critical); }
	.linkbox { border: 1px solid var(--primary); background: var(--accent); border-radius: var(--radius); padding: 14px 16px; margin-bottom: 16px; display: grid; gap: 6px; }
	.linkbox .row { display: flex; gap: 8px; }
	.linkbox input { flex: 1; min-width: 0; }
	:global(.panel) .empty { padding: 20px; }
	@media (max-width: 900px) { .add { grid-template-columns: 1fr; } .acts { white-space: normal; } }
</style>
