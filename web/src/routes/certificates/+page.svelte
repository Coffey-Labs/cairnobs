<script lang="ts">
	import { page } from '$app/state';
	import { replaceState } from '$app/navigation';
	import Icon from '$lib/Icon.svelte';
	import Status from '$lib/Status.svelte';
	import Pager from '$lib/Pager.svelte';
	import { get, send, SignedOut, type Action, type Certificate, type CertReload, type CertRenew } from '$lib/api';
	import { live } from '$lib/live.svelte';

	// The certificate inventory (docs/cert-inventory.md): every certificate
	// obs-beacond knows of, one row each however many places use it,
	// soonest end first. Filters live in the address.
	const limit = 200;
	const u = page.url.searchParams;
	let within = $state(u.get('within') ?? '');
	let kind = $state(u.get('kind') ?? '');
	let status = $state(u.get('status') ?? '');
	let typed = $state(u.get('q') ?? '');
	let q = $state(u.get('q') ?? '');
	let offset = $state(0);
	let rows = $state<Certificate[]>([]);
	let total = $state(0);
	let counts = $state<Record<string, number>>({});
	let kinds = $state<Record<string, number>>({});
	let open = $state<string | null>(null);
	let error = $state('');
	let loaded = $state(false);
	let canAct = $state(false);
	let asking = $state('');
	let askError = $state<Record<string, string>>({});
	$effect(() => {
		const t = typed;
		const d = setTimeout(() => { if (t.trim() !== q) { q = t.trim(); offset = 0; } }, 250);
		return () => clearTimeout(d);
	});
	const params = () => {
		const p = new URLSearchParams();
		for (const [k, v] of [['within', within], ['kind', kind], ['status', status], ['q', q]]) if (v) p.set(k, v);
		return p;
	};
	async function load(p: URLSearchParams, offset: number) {
		try {
			const qp = new URLSearchParams(p);
			qp.set('offset', String(offset));
			qp.set('limit', String(limit));
			const r = await get<{ certificates: Certificate[]; total: number; counts: Record<string, number>; kinds: Record<string, number>; can_act: boolean }>('/api/v1/certificates?' + qp);
			rows = r.certificates;
			canAct = r.can_act;
			total = r.total;
			counts = r.counts;
			kinds = r.kinds;
			error = '';
			loaded = true;
		} catch (e) {
			if (e instanceof SignedOut) void live.checkSession();
			else error = String(e);
		}
	}
	$effect(() => {
		const p = params();
		const o = offset;
		try { replaceState('/certificates' + (p.size ? '?' + p : ''), {}); } catch { /* before the router is up */ }
		void load(p, o);
		const t = setInterval(() => void load(p, o), 30_000);
		return () => clearInterval(t);
	});
	// While a reload or renewal waits for its agent, look every 3 seconds.
	const pending = $derived(rows.some((c) => [...(c.reloads ?? []), ...(c.renews ?? [])].some((r) => busy(r))));
	$effect(() => {
		if (!pending) return;
		const p = params();
		const o = offset;
		const t = setInterval(() => void load(p, o), 3_000);
		return () => clearInterval(t);
	});
	const rkey = (r: CertReload) => `reload ${r.host} ${r.unit} ${r.addr}:${r.port}`;
	const nkey = (r: CertRenew) => `renew ${r.source} ${r.namespace}/${r.certificate}`;
	async function ask(key: string, url: string, body: unknown) {
		asking = key;
		askError = { ...askError, [key]: '' };
		try {
			await send(url, 'POST', body);
			await load(params(), offset);
		} catch (e) {
			askError = { ...askError, [key]: e instanceof Error ? e.message : String(e) };
		} finally {
			asking = '';
		}
	}
	const reload = (r: CertReload) => ask(rkey(r), '/api/v1/certificates/reload', { host: r.host, unit: r.unit, addr: r.addr, port: r.port });
	const renew = (r: CertRenew) => ask(nkey(r), '/api/v1/certificates/renew', { source: r.source, namespace: r.namespace, certificate: r.certificate });
	const busy = (r: { last?: Action }) => r.last?.state === 'waiting' || r.last?.state === 'taken';
	const time = (s?: string) => (s ? new Date(s).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) : '');
	// agent: whose agent; doing: what it's doing while it has it.
	const lastWord = (l: Action | undefined, agent: string, doing: string) => {
		if (!l) return '';
		switch (l.state) {
			case 'waiting': return `Asked at ${time(l.created)} by ${l.by}; waiting for ${agent}'s agent.`;
			case 'taken': return `${agent}'s agent is ${doing}…`;
			case 'done': return `Done at ${time(l.at)}: ${l.detail}`;
			case 'failed': return `Not done (${time(l.at)}): ${l.detail}`;
			case 'expired': return `Asked at ${time(l.created)}, but ${agent}'s agent didn't take it within 10 minutes: is it running?`;
		}
	};
	const set = (fn: () => void) => { fn(); offset = 0; open = null; };
	const clear = () => set(() => { within = ''; kind = ''; status = ''; typed = ''; q = ''; });
	const windows = [['', 'Any time'], ['7', 'Within 7 days'], ['21', 'Within 21 days'], ['90', 'Within 90 days']] as const;
	const kindNames: Record<string, string> = { host: 'Hosts', file: 'Files', vault: 'Vault PKI', check: 'Site checks', cloudflare: 'Cloudflare', akamai: 'Akamai', cluster: 'Clusters' };
	const left = (c: Certificate) => (c.days < 0 ? `ended ${-c.days} days ago` : c.days === 0 ? 'today' : c.days === 1 ? '1 day' : `${c.days} days`);
	const date = (s: string) => s.slice(0, 10);
</script>

<svelte:head><title>Cairn OBS · Certificates</title></svelte:head>

<div class="phead">
	<span class="tile"><Icon name="lock" /></span>
	<div><h1>Certificates</h1><p>Every certificate Cairn OBS has seen, once each however many places use it, the soonest to end first. Alerts stay with where each was found.</p></div>
</div>

{#if error}<p class="foot-note" style="color:var(--critical)">{error}</p>{/if}

<div class="filters">
	<Icon name="filter" />
	<input class="input" placeholder="Name, issuer, place or fingerprint" aria-label="Find a certificate" bind:value={typed} style="width:260px" />
	{#each windows as [w, label]}<button class="chip" class:on={within === w} onclick={() => set(() => (within = w))}>{label}</button>{/each}
	<button class="chip" class:on={status === 'problems'} onclick={() => set(() => (status = status === 'problems' ? '' : 'problems'))}>Need a look <b>{(counts.warning ?? 0) + (counts.critical ?? 0)}</b></button>
</div>
<div class="filters kinds">
	<button class="chip" class:on={kind === ''} onclick={() => set(() => (kind = ''))}>Everywhere <b>{counts.all ?? 0}</b></button>
	{#each Object.keys(kindNames) as k}
		{#if kinds[k]}<button class="chip" class:on={kind === k} onclick={() => set(() => (kind = k))}>{kindNames[k]} <b>{kinds[k]}</b></button>{/if}
	{/each}
	{#if counts.weak}<span class="sub">{counts.weak} weak</span>{/if}
	{#if within || kind || status || q}<button class="btn sm" onclick={clear}>Clear</button>{/if}
</div>

<div class="tbl-wrap">
	<table class="tbl">
		<thead><tr><th>State</th><th>Certificate</th><th>Ends</th><th>Issuer</th><th>Key</th><th>Where</th></tr></thead>
		<tbody>
			{#each rows as c (c.fingerprint || c.subject + c.not_after + c.places[0]?.where)}
				{@const id = c.fingerprint || c.subject + c.not_after + c.places[0]?.where}
				<tr class="row" class:sel={open === id} onclick={() => (open = open === id ? null : id)}>
					<td><Status s={c.status} word={c.status === 'warning' && c.days > 21 ? (c.not_reloaded?.length ? 'Not reloaded' : c.wrong?.length ? 'Served wrong' : c.weak ? 'Weak' : c.word) : c.word} /></td>
					<td><span class="mono nm">{c.subject}</span>{#if c.names?.length > 1}<div class="sub">and {c.names.length - 1} more {c.names.length === 2 ? 'name' : 'names'}</div>{/if}</td>
					<td class="mono nowrap">{date(c.not_after)} <span class="sub">{left(c)}</span></td>
					<td class="sub">{c.issuer || '—'}</td>
					<td class="sub nowrap">{c.key || '—'}{#if c.weak}{' '}<b class="weak">weak</b>{/if}</td>
					<td class="sub">{c.places[0]?.where}{#if c.place_count > 1}{' '}<span class="more-n">+{c.place_count - 1}</span>{/if}</td>
				</tr>
				{#if open === id}
					<tr class="more"><td colspan="6">
						<dl>
							<dt>State</dt><dd>{c.word}{#if c.weak}; weak: {c.weak}{/if}</dd>
							{#if c.not_reloaded?.length}<dt>Not reloaded</dt><dd>A renewed one is in <span class="mono">{c.renewed}</span>; {c.not_reloaded.join(', ')} still {c.not_reloaded.length === 1 ? 'serves' : 'serve'} this one. Reload the service.</dd>
								<dt>Reload</dt><dd>
									{#each c.reloads ?? [] as r (rkey(r))}
										<div class="reload">
											{#if live.admin && canAct}
												<button class="btn sm" disabled={asking === rkey(r) || busy(r)} onclick={() => void reload(r)}>Reload {r.unit} on {r.host}</button>
											{:else}
												<span>{r.host}'s agent may reload <span class="mono">{r.unit}</span>{live.admin ? '; asking needs a database.' : '; an admin can ask it to.'}</span>
											{/if}
											<span class="sub">for {r.where}</span>
											{#if askError[rkey(r)]}<div class="bad">{askError[rkey(r)]}</div>
											{:else if r.last}<div class:bad={r.last.state === 'failed' || r.last.state === 'expired'} class:ok={r.last.state === 'done'} class:sub={busy(r)}>{lastWord(r.last, r.host, 'reloading ' + r.unit)}</div>{/if}
										</div>
									{:else}
										<span class="sub">To let Cairn OBS do it, list the unit and its certificate files in the host's <span class="mono">cert_reload</span> (agent.yaml). An admin then presses a button here; nothing reloads on its own.</span>
									{/each}
								</dd>
							{/if}
							{#if c.wrong?.length}<dt>Served wrong</dt><dd>{#each c.wrong as w}<div>{w}</div>{/each}</dd>{/if}
							{#if c.renews?.length}
								<dt>Renew</dt><dd>
									{#each c.renews as r (nkey(r))}
										<div class="reload">
											{#if live.admin && canAct}
												<button class="btn sm" disabled={asking === nkey(r) || busy(r)} onclick={() => void renew(r)}>Renew {r.certificate} now</button>
											{:else}
												<span>{r.source}'s cluster agent may ask cert-manager to renew it{live.admin ? '; asking needs a database.' : '; an admin can ask.'}</span>
											{/if}
											<span class="sub">{r.where}, as <span class="mono">cmctl renew</span> would</span>
											{#if askError[nkey(r)]}<div class="bad">{askError[nkey(r)]}</div>
											{:else if r.last}<div class:bad={r.last.state === 'failed' || r.last.state === 'expired'} class:ok={r.last.state === 'done'} class:sub={busy(r)}>{lastWord(r.last, r.source, 'asking cert-manager')}</div>{/if}
										</div>
									{/each}
								</dd>
							{/if}
							{#if c.names?.length}<dt>Names</dt><dd class="mono">{c.names.join(', ')}</dd>{/if}
							<dt>Valid</dt><dd class="mono">{date(c.not_before)} to {date(c.not_after)}</dd>
							{#if c.renewals}<dt>Its names have had</dt><dd>{c.renewals} {c.renewals === 1 ? 'certificate' : 'certificates'}</dd>{/if}
							{#if c.sig}<dt>Signature</dt><dd class="mono">{c.sig}</dd>{/if}
							{#if c.fingerprint}<dt>SHA-256</dt><dd class="mono sub">{c.fingerprint}</dd>{/if}
							<dt>Seen at</dt><dd>
								{#each c.places as p}<div><a href={p.href}>{p.where}</a> <span class="sub">({kindNames[p.kind] ?? p.kind})</span></div>{/each}
								{#if c.place_count > c.places.length}<div class="sub">and {c.place_count - c.places.length} more</div>{/if}
							</dd>
						</dl>
					</td></tr>
				{/if}
			{:else}
				<tr><td colspan="6" class="empty">{!loaded ? 'Loading…' : counts.all ? 'Nothing matches.' : 'No certificates yet: they come from hosts\' ports, site checks, Cloudflare, Akamai, clusters and Vault.'}</td></tr>
			{/each}
		</tbody>
	</table>
</div>
<Pager bind:offset {limit} {total} what="certificates" />
<p class="foot-note">Refreshes every 30 seconds. Open a row for every place it's used.</p>

<style>
	.kinds { margin-top: -6px; }
	.row { cursor: pointer; }
	.row.sel td { background: var(--accent); }
	.nm { font-weight: 600; }
	.nowrap { white-space: nowrap; }
	.weak { color: var(--warning); font-weight: 600; }
	.more-n { font-weight: 600; color: var(--foreground); }
	.more td { background: color-mix(in oklab, var(--muted) 35%, var(--card)); }
	dl { display: grid; grid-template-columns: max-content 1fr; gap: 6px 16px; margin: 4px 0; font-size: 12.5px; }
	dt { color: var(--muted-fg); }
	dd { margin: 0; overflow-wrap: anywhere; }
	.reload { display: flex; flex-wrap: wrap; align-items: center; gap: 4px 10px; margin-bottom: 6px; }
	.reload > div { flex-basis: 100%; }
	.bad { color: var(--critical); }
	.ok { color: var(--good); }
</style>
