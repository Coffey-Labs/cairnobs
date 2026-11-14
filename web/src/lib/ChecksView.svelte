<script lang="ts">
	// The checks table. On the monitoring side it's read-only; under
	// Configure an admin adds, edits, exports and imports checks there.
	import Icon from '$lib/Icon.svelte';
	import Panel from '$lib/Panel.svelte';
	import Status from '$lib/Status.svelte';
	import SectionTools from '$lib/SectionTools.svelte';
	import { get, send, SignedOut, type CheckForm, type Checks, type CheckResult, type CheckSettings, type Severity } from '$lib/api';
	import { live } from '$lib/live.svelte';
	import { ago, utc } from '$lib/format';

	let { configure = false }: { configure?: boolean } = $props();

	let data = $state<Checks | null>(null);
	let settings = $state<CheckSettings | null>(null);
	let error = $state('');

	const admin = $derived(live.session?.role === 'admin');
	const editable = $derived(configure && admin && data?.source === 'ui');

	async function load() {
		try {
			data = await get<Checks>('/api/v1/checks');
			if (configure && admin && data.source === 'ui') settings = await get<CheckSettings>('/api/v1/settings/checks');
			error = '';
		} catch (e) {
			if (e instanceof SignedOut) void live.checkSession();
			else error = String(e);
		}
	}
	$effect(() => {
		void load();
		const t = setInterval(() => void load(), 10000);
		return () => clearInterval(t);
	});

	const hosts = $derived([...new Set((data?.checks ?? []).flatMap((c) => c.from))].sort());
	const rank: Record<string, number> = { critical: 0, warning: 1, waiting: 2, good: 3 };
	const rows = $derived([...(data?.checks ?? [])].sort((a, b) => (rank[a.status] ?? 9) - (rank[b.status] ?? 9)));
	const down = $derived(rows.filter((c) => c.word === 'Down').length);
	const KIND: Record<string, string> = { http: 'Web', tcp: 'Port', dns: 'DNS', ping: 'Ping', rbl: 'Lists' };
	function cell(r: CheckResult): string {
		const ms = r.ms >= 100 ? Math.round(r.ms) : r.ms.toFixed(1);
		let s = `${ms} ms`;
		if (r.status) s = `${r.status} · ${s}`;
		if (r.loss_pct) s += ` · ${r.loss_pct}% lost`;
		return s;
	}
	const certSev = (d: number): Severity => (d < 7 ? 'critical' : d < 21 ? 'warning' : 'good');

	// Editing (only when the checks aren't set in a file).
	const blank = (): CheckForm => ({ id: '', name: '', kind: 'http', target: '', lists: [], status: null, contains: '', tls: false, type: '', server: '',
		every_s: null, timeout_s: null, from: [], severity: '', tls_warn_days: null, tls_critical_days: null });
	// 0 from the server means "the default": an empty field showing it.
	const unset = (f: CheckForm): CheckForm => ({ ...f, status: f.status || null, every_s: f.every_s || null, timeout_s: f.timeout_s || null,
		tls_warn_days: f.tls_warn_days || null, tls_critical_days: f.tls_critical_days || null });
	let form = $state<CheckForm | null>(null);
	let isNew = $state(false);
	let lists = $state('');
	let formError = $state('');
	let busy = $state(false);
	let confirming = $state(false);
	let formEl = $state<HTMLElement>();

	function edit(id: string | null) {
		const f = id ? settings?.checks.find((c) => c.id === id) : null;
		form = f ? unset(structuredClone($state.snapshot(f))) : blank();
		isNew = !f;
		lists = form.lists.join(', ');
		formError = '';
		confirming = false;
		requestAnimationFrame(() => formEl?.scrollIntoView({ behavior: 'smooth', block: 'nearest' }));
	}
	async function save(e: SubmitEvent) {
		e.preventDefault();
		if (!form) return;
		busy = true;
		formError = '';
		try {
			const body = { ...form, lists: form.kind === 'rbl' ? lists.split(/[\s,]+/).filter(Boolean) : [] };
			await send(`/api/v1/settings/checks/${encodeURIComponent(form.id.trim())}`, 'PUT', body);
			form = null;
			await load();
		} catch (e) {
			formError = e instanceof Error ? e.message : String(e);
		} finally {
			busy = false;
		}
	}
	async function remove() {
		if (!form) return;
		busy = true;
		try {
			await send(`/api/v1/settings/checks/${encodeURIComponent(form.id)}`, 'DELETE');
			form = null;
			await load();
		} catch (e) {
			formError = e instanceof Error ? e.message : String(e);
		} finally {
			busy = false;
		}
	}
	function toggleHost(h: string) {
		if (!form) return;
		form.from = form.from.includes(h) ? form.from.filter((x) => x !== h) : [...form.from, h].sort();
	}
	const TARGET: Record<string, { label: string; ph: string }> = {
		http: { label: 'URL', ph: 'https://example.org/' },
		tcp: { label: 'Host and port', ph: 'mail.example.org:993' },
		dns: { label: 'Name to resolve', ph: 'example.org' },
		ping: { label: 'Host or address', ph: 'example.org' },
		rbl: { label: 'IPv4 address to look up', ph: '192.0.2.10' }
	};
	const certs = $derived(form?.kind === 'http' || (form?.kind === 'tcp' && form.tls));
</script>

<svelte:head><title>Cairn OBS · {configure ? 'Configure checks' : 'Checks'}</title></svelte:head>

<div class="phead">
	<span class="tile"><Icon name="check" /></span>
	<div>
		<h1>Checks</h1>
		<p>Websites, ports, certificates, DNS and ping, run by every host's agent. Something is down only when two hosts agree; one host failing alone is shown here but doesn't alert.</p>
	</div>
	{#if !configure && live.seesAll}
		<div class="acts"><a class="btn sm" href="/configure/checks"><Icon name="settings" /> Configure</a></div>
	{:else if configure && live.seesAll && data?.source}
		<div class="acts">
			<SectionTools section="checks" what="checks" count={settings?.checks.length ?? 0} {editable} onchange={() => { form = null; void load(); }} />
			{#if editable}<button class="btn sm pri" onclick={() => edit(null)}><Icon name="plus" /> Add check</button>{/if}
		</div>
	{/if}
</div>

{#if error}<p class="foot-note" style="color:var(--critical)">{error}</p>{/if}

{#if form && settings}
	<div bind:this={formEl} class="mb">
		<Panel title={isNew ? 'Add a check' : `Edit ${form.id}`}>
			<form class="cf" onsubmit={save}>
				<label>Id <input bind:value={form.id} required maxlength="64" pattern={'[A-Za-z0-9][A-Za-z0-9 ._\\-]*'} disabled={!isNew} autocomplete="off" placeholder="site" /><span class="hint">Names its alerts; can't change later.</span></label>
				<label>Name <input bind:value={form.name} maxlength="80" placeholder={form.id || 'Shown on this page and in alerts'} autocomplete="off" /></label>
				<label>Kind
					<select bind:value={form.kind}>
						<option value="http">Web: an HTTP(S) request</option>
						<option value="tcp">Port: a TCP connection, optionally TLS</option>
						<option value="dns">DNS: resolve a name</option>
						<option value="ping">Ping</option>
						<option value="rbl">Block lists: is an address listed?</option>
					</select>
				</label>
				<label class="wide">{TARGET[form.kind].label} <input class="mono" bind:value={form.target} required placeholder={TARGET[form.kind].ph} autocomplete="off" /></label>
				{#if form.kind === 'http'}
					<label>Expected status <input type="number" min="100" max="599" bind:value={form.status} placeholder="200" /></label>
				{:else if form.kind === 'tcp'}
					<label class="tick"><input type="checkbox" bind:checked={form.tls} /> TLS from the first byte (465, 993, 995)</label>
				{:else if form.kind === 'dns'}
					<label>Record type
						<select bind:value={form.type}>
							<option value="">A (default)</option>
							{#each ['AAAA', 'MX', 'TXT', 'CNAME', 'NS'] as t (t)}<option value={t}>{t}</option>{/each}
						</select>
					</label>
					<label>Resolver <input class="mono" bind:value={form.server} placeholder="the host's own" autocomplete="off" /></label>
				{:else if form.kind === 'rbl'}
					<label class="wide">Lists <input class="mono" bind:value={lists} placeholder="the default lists" autocomplete="off" /><span class="hint">DNS zones, separated by commas.</span></label>
				{/if}
				{#if form.kind === 'http' || form.kind === 'tcp' || form.kind === 'dns'}
					<label class="wide">Must contain <input bind:value={form.contains} autocomplete="off" placeholder={form.kind === 'http' ? 'text in the page' : form.kind === 'tcp' ? 'text in the banner' : 'text in the answer'} /></label>
				{/if}
				<label>Every (seconds) <input type="number" min="30" bind:value={form.every_s} placeholder={form.kind === 'rbl' ? '1800' : '60'} /></label>
				<label>Timeout (seconds) <input type="number" min="1" bind:value={form.timeout_s} placeholder="10" /></label>
				<label>When down
					<select bind:value={form.severity}>
						<option value="">Critical (default)</option>
						<option value="warning">Warning</option>
					</select>
				</label>
				{#if certs}
					<label>Certificate warning (days) <input type="number" min="1" bind:value={form.tls_warn_days} placeholder="21" /></label>
					<label>Certificate critical (days) <input type="number" min="1" bind:value={form.tls_critical_days} placeholder="7" /></label>
				{/if}
				<fieldset class="wide">
					<legend>Run from</legend>
					<div class="hosts">
						{#each settings.hosts as h (h)}
							<label class="tick"><input type="checkbox" checked={form.from.includes(h)} onchange={() => toggleHost(h)} /> <span class="mono">{h}</span></label>
						{/each}
						{#each form.from.filter((h) => !settings?.hosts.includes(h)) as h (h)}
							<label class="tick gone"><input type="checkbox" checked onchange={() => toggleHost(h)} /> <span class="mono">{h}</span> (no longer a host)</label>
						{/each}
					</div>
					<span class="hint">None ticked: every server (workstations run only checks that name them). It's down when two hosts agree.</span>
				</fieldset>
				<div class="wide row">
					<button class="btn pri" type="submit" disabled={busy}>{isNew ? 'Add check' : 'Save'}</button>
					<button class="btn" type="button" onclick={() => (form = null)}>Cancel</button>
					{#if !isNew}
						<span class="grow"></span>
						{#if confirming}
							<button class="btn danger" type="button" disabled={busy} onclick={remove}>Really remove</button>
							<button class="btn" type="button" onclick={() => (confirming = false)}>Keep</button>
						{:else}
							<button class="btn" type="button" onclick={() => (confirming = true)}>Remove</button>
						{/if}
					{/if}
				</div>
				{#if formError}<p class="wide err" role="alert">{formError}</p>{/if}
			</form>
		</Panel>
	</div>
{/if}

{#if data && !data.configured}
	<Panel title="Not set up">
		<p class="sub">Cairn OBS has no database configured, so checks can't be added here. Set <span class="mono">database</span> in obs-beacond.yaml, or point <span class="mono">checks</span> at a checks file. See docs/formats.md.</p>
	</Panel>
{:else if data}
	<Panel title="All checks" aside="{rows.length} checks · {down} down" flush>
		<div class="tbl-wrap flat">
			<table class="tbl">
				<thead><tr><th>Check</th><th>State</th>{#each hosts as h (h)}<th>{h}</th>{/each}<th class="r">Certificate</th>{#if editable}<th></th>{/if}</tr></thead>
				<tbody>
					{#each rows as c (c.id)}
						<tr>
							<td><div class="name">{c.name}</div><div class="sub mono tgt"><span class="kind">{KIND[c.kind] ?? c.kind}</span> {c.target}</div></td>
							<td><Status s={c.status as Severity} word={c.word} />{#if c.detail}<div class="sub det">{c.detail}</div>{/if}</td>
							{#each hosts as h (h)}
								{@const r = c.results[h]}
								<td class="cellr">
									{#if !c.from.includes(h)}<span class="sub">—</span>
									{:else if !r}<Status s="waiting" word="No result" />
									{:else if r.ok}<span title="{r.answer ?? ''} · {ago(r.at)} ({utc(r.at)})"><Status s="good" word={cell(r)} /></span>
									{:else}<span title="{r.error} · {ago(r.at)}"><Status s="critical" word="Failing" /></span><div class="sub det">{r.error}</div>{/if}
								</td>
							{/each}
							<td class="r">{#if c.cert_days != null}<Status s={certSev(c.cert_days)} word="{c.cert_days} days" />{:else}<span class="sub">—</span>{/if}</td>
							{#if editable}<td class="r"><button class="ico" title="Edit {c.name}" aria-label="Edit {c.name}" onclick={() => edit(c.id)}><Icon name="pencil" /></button></td>{/if}
						</tr>
					{:else}
						<tr><td colspan={hosts.length + 4} class="empty">{#if data.source === 'ui'}No checks yet.{#if editable}{' '}Add one, or import a checks file.{/if}{:else}No checks in the checks file.{/if}</td></tr>
					{/each}
				</tbody>
			</table>
		</div>
	</Panel>
	<p class="foot-note">Each host runs its checks on its own schedule and sends the latest results with its report. Redirects aren't followed: a check names the address it means. Certificates warn at 21 days and are critical at 7 unless a check says otherwise.
		{#if data.source === 'file'}These checks are set in <span class="mono">{data.file}</span>, so they're changed there, not here.{:else if configure && admin}Changes reach the agents with their next report and go in the audit log.{/if}</p>
{:else}
	<p class="empty">Loading…</p>
{/if}

<style>
	.flat { border: 0; border-radius: 0; }
	.name { font-weight: 600; }
	.tgt { overflow-wrap: anywhere; max-width: 340px; }
	.kind { display: inline-block; min-width: 34px; color: var(--muted-fg); }
	.det { max-width: 260px; overflow-wrap: anywhere; }
	.cellr { white-space: nowrap; }
	.cellr .det { white-space: normal; }
	:global(.panel) .empty { padding: 20px; }
	.mb { margin-bottom: 20px; }
	.sm { height: 30px; padding: 0 12px; font-size: 13px; }
	.ico { border: 0; background: none; color: var(--muted-fg); cursor: pointer; padding: 4px; border-radius: var(--radius); display: inline-grid; }
	.ico:hover { color: var(--primary); background: var(--muted); }
	.ico :global(svg) { width: 16px; height: 16px; }
	.cf { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 14px 16px; align-items: start; }
	.cf label, .cf fieldset { display: grid; gap: 6px; font-size: 12.5px; color: var(--muted-fg); min-width: 0; }
	.cf .wide { grid-column: 1 / -1; }
	.cf input:not([type=checkbox]), .cf select { height: 34px; padding: 0 10px; border-radius: var(--radius); border: 1px solid var(--input); background: var(--background); color: var(--foreground); font: inherit; font-size: 13px; width: 100%; min-width: 0; }
	.cf input:disabled { opacity: .7; }
	.cf .tick { display: flex; align-items: center; gap: 8px; color: var(--foreground); font-size: 13px; align-self: end; min-height: 34px; }
	.cf .tick.gone { color: var(--critical); }
	.cf fieldset { border: 0; padding: 0; margin: 0; }
	.cf legend { padding: 0; margin-bottom: 6px; }
	.hosts { display: flex; flex-wrap: wrap; gap: 4px 18px; }
	.hosts .tick { min-height: 26px; }
	.hint { font-size: 11.5px; }
	.row { display: flex; gap: 8px; flex-wrap: wrap; align-items: center; }
	.grow { flex: 1; }
	.danger { border-color: var(--critical); color: var(--critical); }
	.err { margin: 0; color: var(--critical); font-size: 13px; }
	@media (max-width: 900px) { .cf { grid-template-columns: 1fr; } }
</style>
