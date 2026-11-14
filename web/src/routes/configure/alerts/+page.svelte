<script lang="ts">
	import Icon from '$lib/Icon.svelte';
	import Panel from '$lib/Panel.svelte';
	import Status from '$lib/Status.svelte';
	import SectionTools from '$lib/SectionTools.svelte';
	import { get, send, SignedOut, type ChannelForm, type ChannelSettings, type Incidents, type WindowForm, type WindowSettings } from '$lib/api';
	import { live } from '$lib/live.svelte';
	import { duration, hmz, utc } from '$lib/format';

	let data = $state<Incidents | null>(null);
	let error = $state('');
	let testing = $state(false);
	let results = $state<{ id: string; type: string; result: string }[] | null>(null);
	let windows = $state<WindowSettings | null>(null);
	const admin = $derived(live.admin);
	const editable = $derived(admin && data?.maintenance_source === 'ui');

	// Maintenance windows, when they aren't set in obs-beacond.yaml.
	let wform = $state<(WindowForm & { minutes: number }) | null>(null);
	let wNew = $state(false);
	let wError = $state('');
	let wBusy = $state(false);
	let wConfirm = $state(false);
	const zones = typeof Intl.supportedValuesOf === 'function' ? Intl.supportedValuesOf('timeZone') : [];
	function editWindow(name: string | null) {
		const w = name ? windows?.windows.find((x) => x.name === name) : null;
		wform = w ? { ...structuredClone($state.snapshot(w)), minutes: Math.round(w.duration_s / 60) }
			: { name: '', hosts: [], daily: '04:00', timezone: Intl.DateTimeFormat().resolvedOptions().timeZone, duration_s: 0, minutes: 20 };
		wNew = !w;
		wError = '';
		wConfirm = false;
	}
	async function saveWindow(e: SubmitEvent) {
		e.preventDefault();
		if (!wform) return;
		wBusy = true;
		wError = '';
		try {
			const { minutes, ...body } = wform;
			await send(`/api/v1/settings/maintenance/${encodeURIComponent(wform.name.trim())}`, 'PUT', { ...body, duration_s: Math.round(minutes * 60) });
			wform = null;
			await load();
		} catch (e) {
			wError = e instanceof Error ? e.message : String(e);
		} finally {
			wBusy = false;
		}
	}
	async function removeWindow() {
		if (!wform) return;
		wBusy = true;
		try {
			await send(`/api/v1/settings/maintenance/${encodeURIComponent(wform.name)}`, 'DELETE');
			wform = null;
			await load();
		} catch (e) {
			wError = e instanceof Error ? e.message : String(e);
		} finally {
			wBusy = false;
		}
	}
	// Alert channels, when they aren't set in obs-beacond.yaml. Secrets are
	// write-only: the form says which are set, and a blank one keeps it.
	let chans = $state<ChannelSettings | null>(null);
	const chEditable = $derived(admin && data?.channels_source === 'ui');
	type CForm = ChannelForm & { toText: string; secrets: Record<string, string>; clear: string[] };
	let cform = $state<CForm | null>(null);
	let cNew = $state(false);
	let cError = $state('');
	let cBusy = $state(false);
	let cConfirm = $state(false);
	const types = [
		{ id: 'ntfy', label: 'ntfy' },
		{ id: 'discord', label: 'Discord' },
		{ id: 'webhook', label: 'Webhook' },
		{ id: 'email', label: 'Email' },
		{ id: 'pagerduty', label: 'PagerDuty' },
		{ id: 'log', label: 'Server log' }
	];
	const secretFields: Record<string, { key: string; label: string; optional?: boolean; hint: string }[]> = {
		ntfy: [
			{ key: 'topic', label: 'Topic', hint: 'Anyone who knows the topic can read the alerts, so make it hard to guess.' },
			{ key: 'token', label: 'Access token', optional: true, hint: 'Only for a server that needs one.' }
		],
		discord: [{ key: 'webhook', label: 'Webhook URL', hint: 'Channel settings → Integrations → Webhooks.' }],
		webhook: [
			{ key: 'url', label: 'URL', hint: 'Cairn OBS POSTs each notice here as JSON.' },
			{ key: 'secret', label: 'Signing secret', optional: true, hint: 'Signs each request: X-Beacon-Signature: sha256=<HMAC>.' }
		],
		email: [{ key: 'password', label: 'Password', hint: 'For the account on the mail relay.' }],
		pagerduty: [{ key: 'routing_key', label: 'Integration key', hint: 'From the PagerDuty service: Integrations → Add an integration → Events API V2.' }],
		log: []
	};
	function editChannel(id: string | null) {
		const c = id ? chans?.channels.find((x) => x.id === id) : null;
		const base: ChannelForm = c ? structuredClone($state.snapshot(c))
			: { id: '', type: 'ntfy', min: 'warning', server: '', mention_critical: false, smtp: '', username: '', from: '', to: [], set: [] };
		cform = { ...base, toText: base.to.join(', '), secrets: {}, clear: [] };
		cNew = !c;
		cError = '';
		cConfirm = false;
	}
	async function saveChannel(e: SubmitEvent) {
		e.preventDefault();
		if (!cform) return;
		cBusy = true;
		cError = '';
		try {
			const { toText, set: _set, ...body } = cform;
			const keys = (secretFields[cform.type] ?? []).map((f) => f.key);
			body.secrets = Object.fromEntries(Object.entries(cform.secrets).filter(([k, v]) => keys.includes(k) && v.trim() !== ''));
			body.to = toText.split(/[\s,;]+/).filter(Boolean);
			await send(`/api/v1/settings/channels/${encodeURIComponent(cform.id.trim())}`, 'PUT', body);
			cform = null;
			await load();
		} catch (e) {
			cError = e instanceof Error ? e.message : String(e);
		} finally {
			cBusy = false;
		}
	}
	async function removeChannel() {
		if (!cform) return;
		cBusy = true;
		try {
			await send(`/api/v1/settings/channels/${encodeURIComponent(cform.id)}`, 'DELETE');
			cform = null;
			await load();
		} catch (e) {
			cError = e instanceof Error ? e.message : String(e);
		} finally {
			cBusy = false;
		}
	}
	// Whether a stored secret shows as set: the type it was stored for, and not being removed.
	const stored = (f: CForm, key: string) => !cNew && f.set.includes(key) && chans?.channels.find((x) => x.id === f.id)?.type === f.type && !f.clear.includes(key);
	function toggleClear(key: string) {
		if (!cform) return;
		cform.clear = cform.clear.includes(key) ? cform.clear.filter((k) => k !== key) : [...cform.clear, key];
	}

	function toggleWindowHost(h: string) {
		if (!wform) return;
		wform.hosts = wform.hosts.includes(h) ? wform.hosts.filter((x) => x !== h) : [...wform.hosts, h].sort();
	}

	async function load() {
		try {
			data = await get<Incidents>('/api/v1/incidents');
			if (admin && data.maintenance_source === 'ui') windows = await get<WindowSettings>('/api/v1/settings/maintenance');
			if (admin && data.channels_source === 'ui') chans = await get<ChannelSettings>('/api/v1/settings/channels');
			error = '';
		} catch (e) {
			if (e instanceof SignedOut) void live.checkSession();
			else error = String(e);
		}
	}
	$effect(() => {
		void load();
		const t = setInterval(() => void load(), 5000);
		return () => clearInterval(t);
	});

	async function test(only = '') {
		testing = true;
		if (!only) results = null;
		try {
			const r = await fetch('/api/v1/alerts/test' + (only ? `?channel=${encodeURIComponent(only)}` : ''), { method: 'POST', credentials: 'same-origin', headers: { 'X-Beacon': '1' } });
			const got: { id: string; type: string; result: string }[] = (await r.json()).channels;
			results = only ? [...(results ?? []).filter((x) => x.id !== only), ...got] : got;
		} catch (e) {
			error = String(e);
		} finally {
			testing = false;
		}
	}
</script>

<svelte:head><title>Cairn OBS · Configure alerts</title></svelte:head>

<div class="phead">
	<span class="tile"><Icon name="bell" /></span>
	<div>
		<h1>Alerts</h1>
		<p>Where alerts go, and when they're held. Problems become alerts once they've lasted {data ? duration(data.for_s) : '2 min'}; critical ones repeat every {data ? duration(data.repeat_s) : '15 min'} until someone acknowledges them on the <a href="/alerts">Alerts</a> page.</p>
	</div>
</div>

{#if error}<p class="foot-note" style="color:var(--critical)">{error}</p>{/if}

{#if data}
	<div>
		<Panel title="Channels" aside="where alerts go">
			{#if live.seesAll && data.channels_source}
				<div class="wacts in">
					<SectionTools section="channels" what="channels" count={chans?.channels.length ?? 0} editable={chEditable} onchange={() => { cform = null; void load(); }} />
					{#if chEditable}<button class="btn sm pri" onclick={() => editChannel(null)}><Icon name="plus" /> Add channel</button>{/if}
				</div>
			{/if}
			{#if cform && chans}
				<form class="wf cf" onsubmit={saveChannel} autocomplete="off">
					<label>Name <input bind:value={cform.id} required maxlength="64" disabled={!cNew} placeholder="phone" /></label>
					<label>Type
						<select bind:value={cform.type}>{#each types as t (t.id)}<option value={t.id}>{t.label}</option>{/each}</select>
					</label>
					<label>Sends
						<select bind:value={cform.min}>
							<option value="warning">warning and up</option>
							<option value="serious">serious and up</option>
							<option value="critical">critical only</option>
						</select>
					</label>
					{#if cform.type === 'ntfy'}
						<label>Server <input bind:value={cform.server} placeholder="https://ntfy.sh" inputmode="url" /></label>
					{:else if cform.type === 'pagerduty'}
						<label>Events API <input bind:value={cform.server} placeholder="https://events.pagerduty.com" inputmode="url" /><small>EU accounts: https://events.eu.pagerduty.com</small></label>
						<p class="wide sub">PagerDuty follows each incident: it opens when Cairn OBS alerts, is acknowledged when someone acknowledges it here, and resolves with it. Repeats are left to PagerDuty's escalation policy. A test sends a change event, which pages nobody.</p>
					{:else if cform.type === 'email'}
						<label>Mail relay <input bind:value={cform.smtp} required placeholder="smtp.example.org:587" /></label>
						<label>From <input bind:value={cform.from} required placeholder="Cairn OBS <obs@example.org>" /></label>
						<label>To <input bind:value={cform.toText} required placeholder="ops@example.org, oncall@example.org" /></label>
						<label>Username <input bind:value={cform.username} placeholder="the From address" /></label>
					{:else if cform.type === 'discord'}
						<label class="tick"><input type="checkbox" bind:checked={cform.mention_critical} /> @here on critical</label>
					{/if}
					{#each secretFields[cform.type] ?? [] as f (f.key)}
						{@const has = stored(cform, f.key)}
						<label class={f.key === 'password' ? '' : 'wide'}>
							<span>{f.label}{#if f.optional}{' '}<i>(optional)</i>{/if}{#if has}{' '}<b class="isset">set</b>{/if}</span>
							<!-- type=password and a non-login autocomplete keep the browser from filling in a saved sign-in. -->
							<input type="password" autocomplete="new-password" spellcheck="false" bind:value={cform.secrets[f.key]}
								required={!f.optional && !has} disabled={cform.clear.includes(f.key)}
								placeholder={has ? 'Kept. Type to replace it.' : f.key === 'webhook' || f.key === 'url' ? 'https://…' : ''} />
							<small>{f.hint}{#if f.optional && has}{' '}<button type="button" class="linkish" onclick={() => toggleClear(f.key)}>{cform.clear.includes(f.key) ? 'Keep it' : 'Remove it'}</button>{/if}</small>
						</label>
					{/each}
					{#if cform.type === 'log'}<p class="wide sub">Notices go to obs-beacond's log, which is handy for trying things out.</p>{/if}
					<div class="wide row">
						<button class="btn pri" type="submit" disabled={cBusy}>{cNew ? 'Add channel' : 'Save'}</button>
						<button class="btn" type="button" onclick={() => (cform = null)}>Cancel</button>
						{#if !cNew}
							<span class="grow"></span>
							{#if cConfirm}
								<button class="btn danger" type="button" disabled={cBusy} onclick={removeChannel}>Really remove</button>
								<button class="btn" type="button" onclick={() => (cConfirm = false)}>Keep</button>
							{:else}
								<button class="btn" type="button" onclick={() => (cConfirm = true)}>Remove</button>
							{/if}
						{/if}
					</div>
					{#if cError}<p class="wide err" role="alert">{cError}</p>{/if}
				</form>
			{/if}
			{#if data.channels.length}
				<div class="tbl-wrap flat">
					<table class="tbl">
						<thead><tr><th>Channel</th><th>Type</th><th>Sends</th>{#if results}<th>Test</th>{/if}{#if admin}<th></th>{/if}</tr></thead>
						<tbody>
							{#each data.channels as c (c.id)}
								{@const r = results?.find((x) => x.id === c.id)}
								<tr>
									<td class="mono">{c.id}</td><td class="sub">{types.find((t) => t.id === c.type)?.label ?? c.type}</td><td class="sub">{c.min === 'critical' ? 'critical only' : `${c.min} and up`}</td>
									{#if results}<td>{#if r?.result === 'sent'}<Status s="good" word="Sent" />{:else if r}<Status s="critical" word={r.result} />{:else}<span class="sub">—</span>{/if}</td>{/if}
									{#if admin}<td class="r nowrap">
										<button class="ico" title="Send a test to {c.id}" aria-label="Send a test to {c.id}" disabled={testing} onclick={() => test(c.id)}><Icon name="send" /></button>
										{#if chEditable}<button class="ico" title="Edit {c.id}" aria-label="Edit {c.id}" onclick={() => editChannel(c.id)}><Icon name="pencil" /></button>{/if}
									</td>{/if}
								</tr>
							{/each}
						</tbody>
					</table>
				</div>
				<div style="margin-top:14px" hidden={!admin}><button class="btn sm" onclick={() => test()} disabled={testing}><Icon name="send" />{testing ? 'Sending…' : 'Send a test to every channel'}</button></div>
			{:else if chEditable}
				<p class="sub">No channels yet, so alerts only show here. Add one to hear about problems on your phone (ntfy), in Discord, through PagerDuty, by webhook or by email.</p>
			{:else if data.channels_source === 'file'}
				<p class="sub">No channels, so alerts only show here. They're set under <span class="mono">alerts.channels</span> in obs-beacond.yaml.</p>
			{:else}
				<p class="sub">No channels configured, so alerts only show here. Add ntfy, Discord, a webhook or email under <span class="mono">alerts.channels</span> in obs-beacond.yaml.</p>
			{/if}
			{#if admin && data.channels_source === 'file' && data.channels.length}<p class="sub fnote">These channels are set in obs-beacond.yaml, so they're changed there.</p>{/if}
		</Panel>
	</div>
	{#if data.maintenance_source}
		<div class="mt">
			<Panel title="Maintenance windows" aside="alerts for these hosts are held while a window is open" flush>
				{#if live.seesAll && data.maintenance_source}
					<div class="wacts">
						<SectionTools section="maintenance" what="maintenance windows" count={windows?.windows.length ?? 0} {editable} onchange={() => { wform = null; void load(); }} />
						{#if editable}<button class="btn sm pri" onclick={() => editWindow(null)}><Icon name="plus" /> Add window</button>{/if}
					</div>
				{/if}
				{#if wform && windows}
					<form class="wf" onsubmit={saveWindow}>
						<label>Name <input bind:value={wform.name} required maxlength="64" disabled={!wNew} autocomplete="off" placeholder="nightly reboot" /></label>
						<label>Every day at <input type="time" bind:value={wform.daily} required /></label>
						<label>Time zone <input bind:value={wform.timezone} list="tz" placeholder="UTC" autocomplete="off" /></label>
						<label>For (minutes) <input type="number" min="1" max="720" bind:value={wform.minutes} required /></label>
						<fieldset class="wide">
							<legend>Hosts held</legend>
							<div class="hosts">
								{#each windows.hosts as h (h)}
									<label class="tick"><input type="checkbox" checked={wform.hosts.includes(h)} onchange={() => toggleWindowHost(h)} /> <span class="mono">{h}</span></label>
								{/each}
								{#each wform.hosts.filter((h) => !windows?.hosts.includes(h)) as h (h)}
									<label class="tick gone"><input type="checkbox" checked onchange={() => toggleWindowHost(h)} /> <span class="mono">{h}</span> (no longer a host)</label>
								{/each}
							</div>
						</fieldset>
						<div class="wide row">
							<button class="btn pri" type="submit" disabled={wBusy}>{wNew ? 'Add window' : 'Save'}</button>
							<button class="btn" type="button" onclick={() => (wform = null)}>Cancel</button>
							{#if !wNew}
								<span class="grow"></span>
								{#if wConfirm}
									<button class="btn danger" type="button" disabled={wBusy} onclick={removeWindow}>Really remove</button>
									<button class="btn" type="button" onclick={() => (wConfirm = false)}>Keep</button>
								{:else}
									<button class="btn" type="button" onclick={() => (wConfirm = true)}>Remove</button>
								{/if}
							{/if}
						</div>
						{#if wError}<p class="wide err" role="alert">{wError}</p>{/if}
						<datalist id="tz">{#each zones as z (z)}<option value={z}></option>{/each}</datalist>
					</form>
				{/if}
				<div class="tbl-wrap flat">
					<table class="tbl">
						<thead><tr><th>Window</th><th>Hosts</th><th>Every day</th><th class="r">State</th>{#if editable}<th></th>{/if}</tr></thead>
						<tbody>
							{#each data.maintenance ?? [] as w (w.name)}
								<tr>
									<td>{w.name}</td>
									<td class="mono sub">{w.hosts.join(', ')}</td>
									<td class="sub">{w.daily} {w.timezone} for {duration(w.duration_s)}{#if !w.active}{' '}<span title={utc(w.next)}>({hmz(w.next)})</span>{/if}</td>
									<td class="r">{#if w.active}<Status s="warning" word="Open now" />{:else}<span class="sub">opens in {duration((new Date(w.next).getTime() - Date.now()) / 1000)}</span>{/if}</td>
									{#if editable}<td class="r"><button class="ico" title="Edit {w.name}" aria-label="Edit {w.name}" onclick={() => editWindow(w.name)}><Icon name="pencil" /></button></td>{/if}
								</tr>
							{:else}
								<tr><td colspan="5" class="empty">No maintenance windows. Add one for a host's nightly reboot or update run, so it doesn't wake anyone.</td></tr>
							{/each}
						</tbody>
					</table>
				</div>
			</Panel>
		</div>
		<p class="foot-note">A held host's new problems wait until its window closes; alerts already open for it stay open rather than resolving while it reboots. Site and mail checks aren't held: if a reboot really takes mail down, that still alerts.{#if data.maintenance_source === 'file'}{' '}These windows are set in obs-beacond.yaml, so they're changed there.{/if}</p>
	{/if}
	{#if !data.stored}<p class="foot-note">obs-beacond has no database configured, so incidents are kept in memory: restarting obs-beacond forgets them, and anything still wrong alerts again after {duration(data.for_s)}.</p>{/if}
{:else}
	<p class="empty">Loading…</p>
{/if}

<style>
	.sm { height: 30px; padding: 0 12px; font-size: 13px; }
	.flat { border: 0; border-radius: 0; }
	:global(.panel) .empty { padding: 20px; }
	.wacts { display: flex; gap: 8px; flex-wrap: wrap; justify-content: flex-end; padding: 0 20px 12px; }
	.ico { border: 0; background: none; color: var(--muted-fg); cursor: pointer; padding: 4px; border-radius: var(--radius); display: inline-grid; }
	.ico:hover { color: var(--primary); background: var(--muted); }
	.ico :global(svg) { width: 16px; height: 16px; }
	.wf { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 14px 16px; padding: 4px 20px 18px; border-bottom: 1px solid var(--border); }
	.wf label, .wf fieldset { display: grid; align-content: start; gap: 6px; font-size: 12.5px; color: var(--muted-fg); min-width: 0; }
	.wf .wide { grid-column: 1 / -1; }
	.wacts.in { padding: 0 0 12px; }
	.cf { grid-template-columns: repeat(2, minmax(0, 1fr)); padding: 4px 0 18px; margin-bottom: 14px; }
	.wf select { height: 34px; padding: 0 8px; border-radius: var(--radius); border: 1px solid var(--input); background: var(--background); color: var(--foreground); font: inherit; font-size: 13px; width: 100%; }
	.wf small { color: var(--muted-fg); font-size: 12px; }
	.wf i { font-style: normal; opacity: .8; }
	.isset { font-weight: 600; color: var(--good, var(--primary)); }
	.linkish { border: 0; background: none; padding: 0; color: var(--primary); cursor: pointer; font: inherit; text-decoration: underline; }
	.fnote { margin: 12px 0 0; }
	.nowrap { white-space: nowrap; }
	.wf input:not([type=checkbox]) { height: 34px; padding: 0 10px; border-radius: var(--radius); border: 1px solid var(--input); background: var(--background); color: var(--foreground); font: inherit; font-size: 13px; width: 100%; min-width: 0; }
	.wf input:disabled { opacity: .7; }
	.wf fieldset { border: 0; padding: 0; margin: 0; }
	.wf legend { padding: 0; margin-bottom: 6px; }
	.hosts { display: flex; flex-wrap: wrap; gap: 4px 18px; }
	.wf .tick { display: flex; align-items: center; gap: 8px; color: var(--foreground); font-size: 13px; min-height: 26px; }
	.wf .tick.gone { color: var(--critical); }
	.row { display: flex; gap: 8px; flex-wrap: wrap; align-items: center; }
	.grow { flex: 1; }
	.danger { border-color: var(--critical); color: var(--critical); }
	.err { margin: 0; color: var(--critical); font-size: 13px; }
	@media (max-width: 900px) { .wf { grid-template-columns: 1fr; } }
</style>
