<script lang="ts">
	// What pages in one cluster (docs/cluster-watcher.md, What pages, and
	// who decides). Out of the box only the cluster and its nodes page; a
	// workload's problems are warnings on the pages. Adopting a namespace,
	// or one controller or VM, makes its problems page. Each change is
	// saved as it's made: the list is paged, and can run to thousands.
	import { page } from '$app/state';
	import Icon from '$lib/Icon.svelte';
	import Panel from '$lib/Panel.svelte';
	import Pager from '$lib/Pager.svelte';
	import Status from '$lib/Status.svelte';
	import { get, send, SignedOut, type Severity } from '$lib/api';
	import { live } from '$lib/live.svelte';

	type Ns = { name: string; controllers: number; vms: number; problems: number; alert: string };
	type Set = { name: string; alert: string; stopped: boolean };
	type Row = { name: string; kind: string; namespace: string; label: string; state: string; word: string; severity: Severity; annotated: string; set: Set | null; effective: string };
	type View = { source: string; file: string; namespaces: Ns[]; workloads: Row[]; total: number; missing: Set[]; adopted: number };

	const limit = 50;
	const name = $derived(page.params.name ?? '');
	let data = $state<View | null>(null);
	let notFound = $state(false);
	let error = $state('');
	let busy = $state(false);
	let ns = $state('');
	let typed = $state('');
	let q = $state('');
	let offset = $state(0);

	$effect(() => {
		const t = typed;
		const d = setTimeout(() => { if (t.trim() !== q) { q = t.trim(); offset = 0; } }, 250);
		return () => clearTimeout(d);
	});

	async function load(n: string, ns: string, q: string, offset: number) {
		try {
			const p = new URLSearchParams({ offset: String(offset), limit: String(limit) });
			if (ns) p.set('ns', ns);
			if (q) p.set('q', q);
			data = await get<View>(`/api/v1/settings/clusters/${encodeURIComponent(n)}/adopt?${p}`);
			notFound = false;
		} catch (e) {
			if (e instanceof SignedOut) void live.checkSession();
			else if (String(e).includes(': 404 ')) notFound = true;
			else error = String(e);
		}
	}
	$effect(() => {
		const args = [name, ns, q, offset] as const;
		void load(...args);
		const t = setInterval(() => void load(...args), 15_000);
		return () => clearInterval(t);
	});
	const reload = () => load(name, ns, q, offset);

	const editable = $derived(live.admin && data?.source === 'ui');
	const nsWord: Record<string, string> = { '': 'Default: warnings only', page: 'Page', warn: 'Warn only', off: 'Off' };
	const inForce: Record<string, string> = { page: 'Pages', warn: 'Warns', off: 'Off' };

	async function change(body: { namespace?: string; workload?: string; alert: string; stopped?: boolean }) {
		busy = true;
		error = '';
		try {
			await send(`/api/v1/settings/clusters/${encodeURIComponent(name)}/adopt`, 'PUT', body);
			await reload();
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		} finally {
			busy = false;
		}
	}
	// A row's choice as one select value: a VM can also be adopted as
	// stopped on purpose, which still pages if it fails.
	const choice = (r: Row) => (r.set ? (r.set.stopped ? 'stopped' : r.set.alert || 'page') : '');
	const choose = (r: Row, v: string) =>
		change({ workload: r.name, alert: v === 'stopped' ? 'page' : v, stopped: v === 'stopped' });
	const annotated = (r: Row) => (r.annotated ? nsWord[r.annotated] ?? r.annotated : '');
</script>

<svelte:head><title>Cairn OBS · Configure {name}</title></svelte:head>

<p class="crumb"><a href="/configure/clusters">Clusters</a> / <span class="mono">{name}</span></p>
<div class="phead">
	<span class="tile"><Icon name="cluster" /></span>
	<div>
		<h1>{name}</h1>
		<p>What pages. Out of the box, only the cluster and its nodes page; problems with workloads show as warnings. Adopt a namespace, or a single controller or VM, and its problems page. Teams can set the same from the cluster with the <span class="mono">obs.cairnobs.org/alert</span> annotation (<span class="mono">page</span>, <span class="mono">warn</span> or <span class="mono">off</span>); what's set here for a workload wins over it.</p>
	</div>
	<div class="acts"><a class="btn sm" href="/workloads?source={encodeURIComponent(name)}"><Icon name="eye" /> Watch</a></div>
</div>

{#if notFound}
	<p class="empty">No cluster called {name}. <a href="/configure/clusters">Back to the clusters</a>.</p>
{:else if data}
	{#if error}<p class="foot-note" style="color:var(--critical)">{error}</p>{/if}
	{#if data.source === 'file'}
		<p class="sub note">This is set in <span class="mono">{data.file}</span> (<span class="mono">clusters:</span>), so it's changed there.</p>
	{/if}

	{#if data.missing.length}
		<Panel title="Adopted, but not in the cluster" aside={String(data.missing.length)} flush>
			<div class="tbl-wrap flat">
				<table class="tbl">
					<tbody>
						{#each data.missing as m (m.name)}
							<tr>
								<td><span class="mono">{m.name}</span></td>
								<td class="sub">Reported as missing{m.alert === 'off' ? ', but alerting is off' : ''}. If it's gone on purpose, take it off.</td>
								<td class="acts">{#if editable}<button class="btn sm" disabled={busy} onclick={() => change({ workload: m.name, alert: '' })}>Take it off</button>{/if}</td>
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
		</Panel>
		<div class="mt"></div>
	{/if}

	<Panel title="Namespaces" aside={String(data.namespaces.length)} flush>
		<div class="tbl-wrap flat nslist">
			<table class="tbl">
				<thead><tr><th>Namespace</th><th>Controllers</th><th>VMs</th><th>Problems</th><th>Alerting</th></tr></thead>
				<tbody>
					{#each data.namespaces as n (n.name)}
						<tr class:on={ns === n.name}>
							<td><button class="link mono" onclick={() => { ns = ns === n.name ? '' : n.name; offset = 0; }} title="Show its workloads below">{n.name}</button></td>
							<td class="sub">{n.controllers}</td>
							<td class="sub">{n.vms}</td>
							<td class="sub">{n.problems || ''}</td>
							<td>
								{#if editable}
									<select class="input" aria-label="Alerting for {n.name}" value={n.alert} disabled={busy} onchange={(e) => change({ namespace: n.name, alert: e.currentTarget.value })}>
										{#each Object.entries(nsWord) as [v, w]}<option value={v}>{w}</option>{/each}
									</select>
								{:else}<span class="sub">{nsWord[n.alert] ?? n.alert}</span>{/if}
							</td>
						</tr>
					{:else}
						<tr><td colspan="5" class="empty">Nothing from this cluster yet.</td></tr>
					{/each}
				</tbody>
			</table>
		</div>
	</Panel>

	<div class="mt">
		<Panel title="Controllers and VMs" aside="{data.adopted} adopted" flush>
			<div class="bar">
				<input class="input" placeholder="Find by name" aria-label="Find a controller or VM" bind:value={typed} />
				{#if ns}<button class="chip on" onclick={() => { ns = ''; offset = 0; }}>{ns} <Icon name="off" size={12} /></button>{/if}
			</div>
			<div class="tbl-wrap flat">
				<table class="tbl">
					<thead><tr><th>Name</th><th>Now</th><th>From the cluster</th><th>Set here</th><th>In force</th></tr></thead>
					<tbody>
						{#each data.workloads as r (r.name)}
							<tr>
								<td><span class="nm mono">{r.label}</span> <span class="sub">in {r.namespace}</span></td>
								<td><Status s={r.severity} word={r.word} /></td>
								<td class="sub">{annotated(r)}</td>
								<td>
									{#if editable}
										<select class="input" aria-label="Alerting for {r.label} in {r.namespace}" value={choice(r)} disabled={busy} onchange={(e) => choose(r, e.currentTarget.value)}>
											<option value="">Not set</option>
											<option value="page">{r.kind === 'vm' ? 'Page; should run' : 'Page'}</option>
											{#if r.kind === 'vm'}<option value="stopped">Page; stopped on purpose</option>{/if}
											<option value="warn">Warn only</option>
											<option value="off">Off</option>
										</select>
									{:else}<span class="sub">{r.set ? (r.set.stopped ? 'Page; stopped on purpose' : nsWord[r.set.alert || 'page']) : 'Not set'}</span>{/if}
								</td>
								<td class="sub">{inForce[r.effective] ?? r.effective}</td>
							</tr>
						{:else}
							<tr><td colspan="5" class="empty">{q || ns ? 'Nothing matches.' : 'No controllers or VMs yet.'}</td></tr>
						{/each}
					</tbody>
				</table>
			</div>
		</Panel>
		<Pager bind:offset {limit} total={data.total} what="controllers and VMs" />
	</div>
	<p class="foot-note">A pod is judged as part of its controller, so adopting the controller covers its pods. An adopted controller or VM that leaves the cluster is reported as missing. A VM adopted to run that's stopped pages; one stopped on purpose pages only if it fails.</p>
{/if}

<style>
	.crumb { margin: 0 0 10px; font-size: 13px; color: var(--muted-fg); }
	.crumb a { color: inherit; }
	.note { margin: 0 0 12px; }
	.flat { border: 0; border-radius: 0; }
	.nslist { max-height: 420px; overflow-y: auto; }
	.nslist thead th { position: sticky; top: 0; z-index: 1; background: var(--card); }
	.nm { font-weight: 600; }
	.acts { white-space: nowrap; text-align: right; }
	.sm { height: 30px; padding: 0 10px; font-size: 12.5px; }
	.link { background: none; border: 0; padding: 0; color: inherit; font: inherit; font-weight: 600; cursor: pointer; text-align: left; }
	.link:hover { text-decoration: underline; }
	tr.on td { background: var(--accent); }
	.bar { display: flex; gap: 8px; align-items: center; padding: 10px 12px; border-bottom: 1px solid var(--border); flex-wrap: wrap; }
	.bar .input { width: 240px; max-width: 100%; }
	select.input { height: 30px; }
	:global(.panel) .empty { padding: 20px; }
</style>
