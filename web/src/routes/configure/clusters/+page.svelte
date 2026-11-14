<script lang="ts">
	// Adding a cluster (docs/cluster-watcher.md, Joining): one command, run
	// with access to the cluster, applies the cluster agent's manifest with
	// a one-time join token in its Secret. The agent joins, keeps its
	// certificate in that Secret, and its cluster shows up below.
	import Icon from '$lib/Icon.svelte';
	import Panel from '$lib/Panel.svelte';
	import Status from '$lib/Status.svelte';
	import { get, send, SignedOut, type Severity } from '$lib/api';
	import { live } from '$lib/live.svelte';
	import { ago } from '$lib/format';

	type ClusterAgent = { name: string; source: 'config' | 'joined'; added?: string; added_by?: string; cert_until?: string; status: Severity | ''; word: string };
	type Pending = { id: string; name: string; cluster: boolean; created_by: string; created: string; expires: string };
	type Made = { name: string; command: string; expires: string };

	let clusters = $state<ClusterAgent[]>([]);
	let pending = $state<Pending[]>([]);
	let unavailable = $state('');
	let error = $state('');
	let busy = $state(false);
	let name = $state('');
	let made = $state<Made | null>(null);
	let copied = $state(false);
	let confirming = $state<string | null>(null);

	async function load() {
		try {
			const r = await get<{ clusters: ClusterAgent[]; pending: Pending[]; cluster_join_unavailable: string }>('/api/v1/agents?limit=1');
			clusters = r.clusters;
			pending = r.pending.filter((p) => p.cluster);
			unavailable = r.cluster_join_unavailable;
			error = '';
		} catch (e) {
			if (e instanceof SignedOut) void live.checkSession();
			else error = String(e);
		}
	}
	$effect(() => {
		void load();
		const t = setInterval(load, 10_000); // a cluster that joins shows up without a reload
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
		made = await send<Made>('/api/v1/agents/join', 'POST', { name: name.trim().toLowerCase(), cluster: true });
		copied = false;
		name = '';
	});
	const cancel = (p: Pending) => act(async () => {
		await send(`/api/v1/agents/join/${p.id}`, 'DELETE');
		if (made?.name === p.name) made = null;
	});
	const remove = (c: ClusterAgent) => act(async () => {
		await send(`/api/v1/agents/${encodeURIComponent(c.name)}`, 'DELETE');
		confirming = null;
	});
	async function copy() {
		if (!made) return;
		try {
			await navigator.clipboard.writeText(made.command);
			copied = true;
		} catch {
			copied = false;
		}
	}
	$effect(() => {
		if (made && !pending.some((p) => p.name === made?.name)) made = null;
	});
	const manifest = $derived(typeof location === 'undefined' ? '/cluster.yaml' : location.origin + '/cluster.yaml');
</script>

<svelte:head><title>Cairn OBS · Configure clusters</title></svelte:head>

<div class="phead">
	<span class="tile"><Icon name="cluster" /></span>
	<div>
		<h1>Clusters</h1>
		<p>Each Kubernetes, OpenShift or OKD cluster runs one cluster agent. It reads the cluster with get, list and watch only, and sends its nodes, pods, controllers, jobs and volumes here. Add a cluster and run the command it gives you wherever you run kubectl or oc against that cluster.</p>
	</div>
	<div class="acts"><a class="btn sm" href="/clusters"><Icon name="cluster" /> See them</a></div>
</div>

{#if error}<p class="foot-note" style="color:var(--critical)">{error}</p>{/if}

{#if made}
	<div class="linkbox" role="status">
		<b>Run this against {made.name}, as a cluster admin</b>
		<p class="sub">It works once, until {new Date(made.expires).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}. It makes the <span class="mono">cairn-obs-agent</span> namespace, a read-only ClusterRole and the agent, which joins this server. {made.name} shows up below when it has joined. With OpenShift, <span class="mono">oc apply -f -</span> works the same.</p>
		<div class="row"><input class="mono" readonly value={made.command} onfocus={(e) => e.currentTarget.select()} aria-label="Install command" /><button class="btn sm" onclick={copy}>{copied ? 'Copied' : 'Copy'}</button><button class="btn sm" onclick={() => (made = null)}>Done</button></div>
	</div>
{/if}

{#if live.admin}
<Panel title="Add a cluster">
	{#if unavailable}
		<p class="sub">{unavailable}</p>
	{:else}
		<form class="add" onsubmit={(e) => { e.preventDefault(); void add(); }}>
			<label>Name <input bind:value={name} required maxlength="63" pattern="[a-zA-Z0-9]([a-zA-Z0-9\-]*[a-zA-Z0-9])?" placeholder="prod" autocomplete="off" spellcheck="false" /></label>
			<button class="btn pri" type="submit" disabled={busy}><Icon name="cluster" /> Make the install command</button>
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
								<td><span class="nm mono">{p.name}</span></td>
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
	<Panel title="Watched" aside="{clusters.length} {clusters.length === 1 ? 'cluster' : 'clusters'}" flush>
		<div class="tbl-wrap flat">
			<table class="tbl">
				<thead><tr><th>Cluster</th><th>State</th><th>Added</th><th>Certificate</th><th></th></tr></thead>
				<tbody>
					{#each clusters as c (c.name)}
						<tr>
							<td><a class="nm mono" href="/configure/clusters/{encodeURIComponent(c.name)}">{c.name}</a></td>
							<td>{#if c.status}<Status s={c.status} word={c.word} />{:else}<span class="sub">…</span>{/if}</td>
							<td class="sub">{c.source === 'joined' ? `Joined ${ago(c.added ?? '')}, by ${c.added_by}` : 'In obs-beacond.yaml'}</td>
							<td class="sub">{c.cert_until ? 'Until ' + new Date(c.cert_until).toLocaleDateString() : c.source === 'config' ? 'Token' : 'None'}</td>
							<td class="acts">
								<a class="btn sm" href="/configure/clusters/{encodeURIComponent(c.name)}">What pages</a>
									{#if c.source === 'joined' && live.admin}
									{#if confirming === c.name}
										<button class="btn sm danger" disabled={busy} onclick={() => remove(c)}>Really remove</button>
										<button class="btn sm" onclick={() => (confirming = null)}>Keep</button>
									{:else}
										<button class="btn sm" disabled={busy} onclick={() => (confirming = c.name)}>Remove</button>
									{/if}
								{/if}
							</td>
						</tr>
					{:else}
						<tr><td colspan="5" class="empty">No clusters yet.{#if live.admin && !unavailable} Add the first one above.{/if}</td></tr>
					{/each}
				</tbody>
			</table>
		</div>
	</Panel>
</div>
<p class="foot-note">Removing a cluster revokes its agent's certificate at once and forgets what it sent. To take the agent out of the cluster: <span class="mono">curl -fsSL {manifest} | kubectl delete -f -</span>. Applied instead (<span class="mono">kubectl apply -f -</span>), it upgrades the agent to this server's version.</p>

<style>
	.flat { border: 0; border-radius: 0; }
	.nm { font-weight: 600; color: inherit; text-decoration: none; }
	a.nm:hover { text-decoration: underline; }
	.acts { white-space: nowrap; text-align: right; }
	.acts .btn { margin-left: 4px; }
	.sm { height: 30px; padding: 0 10px; font-size: 12.5px; }
	.danger { border-color: var(--critical); color: var(--critical); }
	input { height: 34px; padding: 0 10px; border-radius: var(--radius); border: 1px solid var(--input); background: var(--background); color: var(--foreground); font: inherit; font-size: 13px; }
	.add { display: grid; grid-template-columns: minmax(180px, 1fr) auto; gap: 12px; align-items: end; }
	.add label { display: grid; gap: 6px; font-size: 12.5px; color: var(--muted-fg); }
	.linkbox { border: 1px solid var(--primary); background: var(--accent); border-radius: var(--radius); padding: 14px 16px; margin-bottom: 16px; display: grid; gap: 6px; }
	.linkbox .row { display: flex; gap: 8px; }
	.linkbox input { flex: 1; min-width: 0; }
	:global(.panel) .empty { padding: 20px; }
	@media (max-width: 900px) { .add { grid-template-columns: 1fr; } .acts { white-space: normal; } }
</style>
