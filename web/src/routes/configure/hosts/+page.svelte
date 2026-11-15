<script lang="ts">
	import Icon from '$lib/Icon.svelte';
	import Panel from '$lib/Panel.svelte';
	import Status from '$lib/Status.svelte';
	import SectionTools from '$lib/SectionTools.svelte';
	import Pager from '$lib/Pager.svelte';
	import { get, send, SignedOut, type SettingsSummary, type Severity } from '$lib/api';
	import { live } from '$lib/live.svelte';
	import { ago } from '$lib/format';

	type Agent = { name: string; source: 'config' | 'joined'; workstation: boolean; added?: string; added_by?: string; cert_until?: string; status: Severity | ''; word: string };
	type Pending = { id: string; name: string; workstation: boolean; cluster: boolean; created_by: string; created: string; expires: string };
	type Made = { name: string; command: string; windows_command?: string; expires: string };

	let agents = $state<Agent[]>([]);
	let total = $state(0);
	const limit = 100;
	let offset = $state(0);
	let typed = $state('');
	let q = $state('');
	$effect(() => {
		const t = typed;
		const d = setTimeout(() => { q = t.trim(); offset = 0; }, 250);
		return () => clearTimeout(d);
	});
	let pending = $state<Pending[]>([]);
	let unavailable = $state('');
	let error = $state('');
	let busy = $state(false);
	let form = $state({ name: '', workstation: false });
	let made = $state<Made | null>(null);
	let copied = $state(false);
	let confirming = $state<string | null>(null);
	let summary = $state<SettingsSummary | null>(null);

	async function load() {
		try {
			const p = new URLSearchParams({ q, offset: String(offset), limit: String(limit) });
			const r = await get<{ agents: Agent[]; total: number; pending: Pending[]; join_unavailable: string }>('/api/v1/agents?' + p);
			agents = r.agents;
			total = r.total;
			pending = r.pending.filter((p) => !p.cluster); // clusters wait on Configure, Clusters
			unavailable = r.join_unavailable;
			summary = await get<SettingsSummary>('/api/v1/settings');
			error = '';
		} catch (e) {
			if (e instanceof SignedOut) void live.checkSession();
			else error = String(e);
		}
	}
	$effect(() => {
		void [q, offset];
		void load();
		const t = setInterval(load, 10_000); // a host that joins shows up without a reload
		return () => clearInterval(t);
	});

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
		made = await send<Made>('/api/v1/agents/join', 'POST', { name: form.name.trim().toLowerCase(), workstation: form.workstation });
		copied = false;
		form = { name: '', workstation: false };
	});
	const cancel = (p: Pending) => act(async () => {
		await send(`/api/v1/agents/join/${p.id}`, 'DELETE');
		if (made?.name === p.name) made = null;
	});
	const remove = (a: Agent) => act(async () => {
		await send(`/api/v1/agents/${encodeURIComponent(a.name)}`, 'DELETE');
		confirming = null;
	});

	// Linux or Windows: which install line the box shows.
	let os = $state<'linux' | 'windows'>('linux');
	const line = $derived(made ? (os === 'windows' && made.windows_command ? made.windows_command : made.command) : '');
	async function copy() {
		if (!made) return;
		try {
			await navigator.clipboard.writeText(line);
			copied = true;
		} catch {
			copied = false;
		}
	}
	// The host has joined (its token is spent): the command box has done its
	// job. The list is paged, so the token going is the sign, not the row.
	$effect(() => {
		if (made && !pending.some((p) => p.name === made?.name)) made = null;
	});
</script>

<svelte:head><title>Cairn OBS · Configure hosts</title></svelte:head>

<div class="phead">
	<span class="tile"><Icon name="hosts" /></span>
	<div>
		<h1>Hosts</h1>
		<p>Every host Cairn OBS watches runs one agent. Add a host here and run the line it gives you on that host: it installs the agent, which proves who it is with a certificate from then on. Open a host to choose what should run on it.</p>
	</div>
	{#if summary?.sections.expected.source}
		<div class="acts tools"><SectionTools section="expected" what="host lists" count={summary.sections.expected.count} editable={live.admin && summary.sections.expected.source === 'ui'} onchange={() => void load()} /></div>
	{/if}
</div>

{#if error}<p class="foot-note" style="color:var(--critical)">{error}</p>{/if}

{#if made}
	<div class="linkbox" role="status">
		<b>Run this on {made.name}, {os === 'windows' ? 'in PowerShell as Administrator' : 'as root'}</b>
		{#if made.windows_command}
			<div class="seg" role="group" aria-label="The host's system">
				<button class="chip" class:on={os === 'linux'} aria-pressed={os === 'linux'} onclick={() => { os = 'linux'; copied = false; }}>Linux</button>
				<button class="chip" class:on={os === 'windows'} aria-pressed={os === 'windows'} onclick={() => { os = 'windows'; copied = false; }}>Windows</button>
			</div>
		{/if}
		<p class="sub">It works once, until {new Date(made.expires).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}. It installs obs-beacon-agent, joins it to this server and starts it. {made.name} shows up below when it has joined.</p>
		<div class="row"><input class="mono" readonly value={line} onfocus={(e) => e.currentTarget.select()} aria-label="Install command" /><button class="btn sm" onclick={copy}>{copied ? 'Copied' : 'Copy'}</button><button class="btn sm" onclick={() => (made = null)}>Done</button></div>
	</div>
{/if}

{#if live.admin}
<Panel title="Add a host">
	{#if unavailable}
		<p class="sub">{unavailable}</p>
	{:else}
		<form class="add" onsubmit={(e) => { e.preventDefault(); void add(); }}>
			<label>Name <input bind:value={form.name} required maxlength="63" pattern="[a-zA-Z0-9]([a-zA-Z0-9\-]*[a-zA-Z0-9])?" placeholder="web1" autocomplete="off" spellcheck="false" /></label>
			<label class="check"><input type="checkbox" bind:checked={form.workstation} /> A workstation: watched, but never pages (it's switched off at night)</label>
			<button class="btn pri" type="submit" disabled={busy}><Icon name="hosts" /> Make the install command</button>
		</form>
	{/if}
</Panel>
{/if}

{#if pending.length}
	<div class="mt">
		<Panel title="Waiting to join" aside={String(pending.length)} flush>
			<div class="tbl-wrap flat">
				<table class="tbl">
					<thead><tr><th>Name</th><th>Made by</th><th>Good until</th><th></th></tr></thead>
					<tbody>
						{#each pending as p (p.id)}
							<tr>
								<td><span class="nm mono">{p.name}</span>{#if p.workstation}<span class="badge">Workstation</span>{/if}</td>
								<td class="sub">{p.created_by}, {ago(p.created)}</td>
								<td class="sub">{new Date(p.expires).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}</td>
								<td class="acts">{#if live.admin}<button class="btn sm" disabled={busy} onclick={() => cancel(p)}>Cancel</button>{/if}</td>
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
		</Panel>
	</div>
{/if}

<div class="mt">
	<Panel title="Watched" aside="{total} {total === 1 ? 'host' : 'hosts'}" flush>
		{#if total > limit || typed || q}
			<div class="find"><input class="input" placeholder="Find a host" aria-label="Find a host" bind:value={typed} style="width:220px" /></div>
		{/if}
		<div class="tbl-wrap flat">
			<table class="tbl">
				<thead><tr><th>Host</th><th>State</th><th>Added</th><th>Certificate</th><th></th></tr></thead>
				<tbody>
					{#each agents as a (a.name)}
						<tr>
							<td><a class="nm mono" href="/configure/hosts/{a.name}">{a.name}</a>{#if a.workstation}<span class="badge">Workstation</span>{/if}</td>
							<td>{#if a.status}<Status s={a.status} word={a.word} />{:else}<span class="sub">…</span>{/if}</td>
							<td class="sub">{a.source === 'joined' ? `Joined ${ago(a.added ?? '')}, by ${a.added_by}` : 'In obs-beacond.yaml'}</td>
							<td class="sub">{a.cert_until ? 'Until ' + new Date(a.cert_until).toLocaleDateString() : a.source === 'config' ? 'Token' : 'None'}</td>
							<td class="acts">
								{#if a.source === 'joined' && live.admin}
									{#if confirming === a.name}
										<button class="btn sm danger" disabled={busy} onclick={() => remove(a)}>Really remove</button>
										<button class="btn sm" onclick={() => (confirming = null)}>Keep</button>
									{:else}
										<button class="btn sm" disabled={busy} onclick={() => (confirming = a.name)}>Remove</button>
									{/if}
								{/if}
							</td>
						</tr>
					{:else}
						<tr><td colspan="5" class="empty">{q ? 'No hosts match.' : 'No hosts yet. Add the first one above.'}</td></tr>
					{/each}
				</tbody>
			</table>
		</div>
	</Panel>
	{#if total > limit}<Pager bind:offset {limit} {total} what="hosts" />{/if}
</div>
<p class="foot-note">Removing a host revokes its certificate at once; its agent keeps running but nothing it sends is taken. Uninstall it there with your package manager. Hosts listed in obs-beacond.yaml are changed there.</p>

<style>
	.flat { border: 0; border-radius: 0; }
	.nm { font-weight: 600; color: inherit; text-decoration: none; }
	a.nm:hover { text-decoration: underline; }
	.acts { white-space: nowrap; text-align: right; }
	.acts .btn { margin-left: 4px; }
	.sm { height: 30px; padding: 0 10px; font-size: 12.5px; }
	.danger { border-color: var(--critical); color: var(--critical); }
	input { height: 34px; padding: 0 10px; border-radius: var(--radius); border: 1px solid var(--input); background: var(--background); color: var(--foreground); font: inherit; font-size: 13px; }
	.add { display: grid; grid-template-columns: minmax(180px, 1fr) 2fr auto; gap: 12px; align-items: end; }
	.add label { display: grid; gap: 6px; font-size: 12.5px; color: var(--muted-fg); }
	.add label.check { display: flex; align-items: center; gap: 8px; padding-bottom: 8px; }
	.add label.check input { height: auto; }
	.badge { margin-left: 8px; padding: 1px 7px; border-radius: 10px; font: 600 10.5px var(--mono); letter-spacing: .06em; text-transform: uppercase; color: var(--muted-fg); border: 1px solid var(--border); }
	.linkbox { border: 1px solid var(--primary); background: var(--accent); border-radius: var(--radius); padding: 14px 16px; margin-bottom: 16px; display: grid; gap: 6px; }
	.linkbox .row { display: flex; gap: 8px; }
	.linkbox .seg { display: flex; gap: 6px; }
	.linkbox input { flex: 1; min-width: 0; }
	:global(.panel) .empty { padding: 20px; }
	@media (max-width: 900px) { .add { grid-template-columns: 1fr; } .acts { white-space: normal; } }
	.find { padding: 12px 16px 0; }
</style>
