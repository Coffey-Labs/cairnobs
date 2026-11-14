<script lang="ts">
	import Icon from '$lib/Icon.svelte';
	import Panel from '$lib/Panel.svelte';
	import Spark from '$lib/Spark.svelte';
	import Status from '$lib/Status.svelte';
	import { get, send, SignedOut, type Severity } from '$lib/api';
	import { live } from '$lib/live.svelte';
	import { bytes, duration } from '$lib/format';

	type Uptime = { id: string; name: string; known_s: number; down_s: number; degraded_s: number; pct: number; incidents: number };
	type Note = { text: string; by: string; at: string };
	type Inc = { id: string; severity: Severity; title: string; host?: string; service?: string; opened: string; resolved?: string; ack_by?: string; to_ack_s?: number; to_fix_s?: number; note?: Note };
	type Cert = { name: string; where: string; source: string; expires: string; days: number };
	type DiskRow = { host: string; mount: string; used: number; total: number; pct: number; per_day: number; full_days?: number; projected?: string };
	type Trend = { host: string; days: string[]; cpu: number[]; cpu_max: number[]; mem: number[] };
	type Mail = { rounds: number; missed: number; median_s: number; p95_s: number; max_s: number; queue_max: number; listed: Uptime[] };
	type Backup = { host: string; expected: number; current: number; problems: string[]; incidents: number };
	type Report = { from: string; to: string; label: string; history_since?: string; services: Uptime[]; hosts: Uptime[]; checks: Uptime[]; incidents: Inc[]; certs: Cert[]; disks: DiskRow[]; trends: Trend[]; mail?: Mail; backups: Backup[] };
	type View = { configured: boolean; error?: string; report?: Report; months?: string[]; timezone?: string; digest?: { weekday: string; at: string; timezone: string; channels: string[]; last: string } | null };

	let periodSel = $state('30d');
	let data = $state<View | null>(null);
	let error = $state('');
	let showAllChecks = $state(false);
	let showAllCerts = $state(false);
	async function load() {
		try {
			data = await get<View>('/api/v1/reports?period=' + encodeURIComponent(periodSel));
			error = '';
		} catch (e) {
			if (e instanceof SignedOut) void live.checkSession();
			else error = String(e);
		}
	}
	$effect(() => {
		const p = new URLSearchParams(location.search).get('period');
		if (p) periodSel = p;
	});
	$effect(() => {
		void periodSel;
		void load();
	});
	function choose(p: string) {
		periodSel = p;
		const u = new URL(location.href);
		u.searchParams.set('period', p);
		history.replaceState(history.state, '', u);
	}

	const r = $derived(data?.report);
	const admin = $derived(live.seesAll); // causes and the digest preview; sending is admins'
	// Reports are cut into days and months in the server's reports timezone
	// (reports.timezone), so they're shown in it too, whoever is looking.
	const tz = $derived(data?.timezone || undefined);
	const dateFmt = $derived(new Intl.DateTimeFormat('en-US', { timeZone: tz, month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', hour12: false }));
	const dayFmt = $derived(new Intl.DateTimeFormat('en-US', { timeZone: tz, month: 'short', day: 'numeric', year: 'numeric' }));
	const tzName = $derived(new Intl.DateTimeFormat('en-US', { timeZone: tz, timeZoneName: 'short' }).formatToParts(new Date(r?.to ?? Date.now())).find((p) => p.type === 'timeZoneName')?.value ?? '');
	const monthName = (m: string) => new Date(m + '-15T12:00:00Z').toLocaleString('en-US', { month: 'long', year: 'numeric', timeZone: 'UTC' });
	const fmtPct = (p: number) => (p < 0 ? '—' : p >= 99.995 ? '100%' : (Math.floor(p * 100) / 100).toFixed(2) + '%');
	const pctSev = (u: Uptime): Severity => (u.pct < 0 ? 'waiting' : u.down_s > 0 ? (u.pct < 99 ? 'critical' : 'serious') : u.degraded_s > 0 ? 'warning' : 'good');
	const certSev = (d: number): Severity => (d < 7 ? 'critical' : d < 21 ? 'warning' : 'good');
	const csv = (section: string) => `/api/v1/reports/csv/${section}?period=${encodeURIComponent(periodSel)}`;
	const checksShown = $derived((r?.checks ?? []).filter((c) => showAllChecks || c.pct < 100 || c.incidents > 0));
	const partial = $derived(r?.history_since && new Date(r.history_since) > new Date(r.from));

	// Incident causes (admins).
	let editing = $state<string | null>(null);
	let draft = $state('');
	let saving = $state(false);
	async function saveNote(id: string) {
		saving = true;
		try {
			await send(`/api/v1/incidents/${encodeURIComponent(id)}/note`, 'PUT', { text: draft });
			editing = null;
			await load();
		} catch (e) {
			error = String(e);
		} finally {
			saving = false;
		}
	}

	// The weekly digest (admins).
	let preview = $state<{ subject: string; text: string; channels: string[] } | null>(null);
	let digestMsg = $state('');
	async function showDigest() {
		digestMsg = '';
		try {
			preview = await get('/api/v1/reports/digest');
		} catch (e) {
			digestMsg = String(e);
		}
	}
	async function sendDigest() {
		digestMsg = 'Sending…';
		try {
			await send('/api/v1/reports/digest', 'POST');
			digestMsg = 'Sent to ' + (preview?.channels ?? data?.digest?.channels ?? []).join(', ') + '.';
		} catch (e) {
			digestMsg = String(e);
		}
	}
</script>

<svelte:head><title>Cairn OBS · Reports</title></svelte:head>

<div class="phead">
	<span class="tile"><Icon name="report" /></span>
	<div>
		<h1>Reports</h1>
		<p>Uptime, incidents, certificates, disk space, mail and backups over a period, in {data?.timezone ? data.timezone.replaceAll('_', ' ') + ' time' : 'the server’s time zone'}. Every section downloads as CSV. Print the page to save it as a PDF.</p>
	</div>
	<div class="acts noprint">
		<select class="sel" value={periodSel} onchange={(e) => choose((e.target as HTMLSelectElement).value)} aria-label="Period">
			<option value="7d">Last 7 days</option>
			<option value="30d">Last 30 days</option>
			{#each data?.months ?? [] as m (m)}<option value={m}>{monthName(m)}</option>{/each}
		</select>
		<button class="btn" onclick={() => window.print()}><Icon name="list" size={15} />Print / PDF</button>
	</div>
</div>

{#if error}<p class="foot-note" style="color:var(--critical)">{error}</p>{/if}

{#if data && !data.configured}
	<Panel title="Not available"><p class="sub">{data.error}</p></Panel>
{:else if r}
	<p class="printhead">Cairn OBS report · {r.label} · {dayFmt.format(new Date(r.from))} to {dayFmt.format(new Date(r.to))} ({tzName})</p>
	{#if partial}<p class="foot-note">Cairn OBS has kept history since {dateFmt.format(new Date(r.history_since!))} {tzName}, so this period is partly before it. Uptime counts only the time it was watching.</p>{/if}

	<div class="grid g2 mt">
		<Panel title="Services" aside="uptime · {r.label}" href={csv('services')} hrefLabel="CSV" flush>
			<div class="tbl-wrap flat">
				<table class="tbl">
					<thead><tr><th>Service</th><th class="r">Uptime</th><th class="r">Down</th><th class="r">Degraded</th><th class="r">Incidents</th></tr></thead>
					<tbody>
						{#each r.services as u (u.id)}
							<tr><td><b>{u.name}</b></td><td class="r"><Status s={pctSev(u)} word={fmtPct(u.pct)} /></td><td class="r mono">{u.down_s ? duration(u.down_s) : '—'}</td><td class="r mono">{u.degraded_s ? duration(u.degraded_s) : '—'}</td><td class="r mono">{u.incidents || '—'}</td></tr>
						{:else}<tr><td colspan="5" class="empty">No history yet.</td></tr>{/each}
					</tbody>
				</table>
			</div>
		</Panel>
		<div class="stack">
			<Panel title="Hosts" aside="uptime" href={csv('hosts')} hrefLabel="CSV" flush>
				<div class="tbl-wrap flat">
					<table class="tbl">
						<thead><tr><th>Host</th><th class="r">Uptime</th><th class="r">Down</th><th class="r">Incidents</th></tr></thead>
						<tbody>
							{#each r.hosts as u (u.id)}<tr><td><b>{u.name}</b></td><td class="r"><Status s={pctSev(u)} word={fmtPct(u.pct)} /></td><td class="r mono">{u.down_s ? duration(u.down_s) : '—'}</td><td class="r mono">{u.incidents || '—'}</td></tr>{/each}
						</tbody>
					</table>
				</div>
			</Panel>
			<Panel title="Backups" aside="now · incidents in the period" flush>
				<div class="tbl-wrap flat">
					<table class="tbl">
						<thead><tr><th>Host</th><th>Now</th><th class="r">Incidents</th></tr></thead>
						<tbody>
							{#each r.backups as b (b.host)}
								<tr><td><b>{b.host}</b></td><td><Status s={b.problems.length ? 'serious' : 'good'} word="{b.current} of {b.expected} current" />{#each b.problems as p (p)}<div class="sub det">{p}</div>{/each}</td><td class="r mono">{b.incidents || '—'}</td></tr>
							{:else}<tr><td colspan="3" class="empty">No backups.yaml.</td></tr>{/each}
						</tbody>
					</table>
				</div>
			</Panel>
		</div>
	</div>

	<div class="mt">
		<Panel title="Incidents" aside="{r.incidents.length} · times to acknowledge and fix" href={csv('incidents')} hrefLabel="CSV" flush>
			<div class="tbl-wrap flat">
				<table class="tbl">
					<thead><tr><th>Opened ({tzName})</th><th>What</th><th class="r">To acknowledge</th><th class="r">To fix</th><th>Cause</th></tr></thead>
					<tbody>
						{#each r.incidents as i (i.id)}
							<tr>
								<td class="mono nowrap">{dateFmt.format(new Date(i.opened))}</td>
								<td><Status s={i.severity} word={i.title} />{#if i.host || i.service}<div class="sub">{[i.service, i.host].filter(Boolean).join(' · ')}</div>{/if}</td>
								<td class="r mono">{i.to_ack_s != null ? duration(i.to_ack_s) : '—'}{#if i.ack_by}<div class="sub">{i.ack_by}</div>{/if}</td>
								<td class="r mono">{i.to_fix_s != null ? duration(i.to_fix_s) : 'open'}</td>
								<td class="cause">
									{#if editing === i.id}
										<textarea rows="2" bind:value={draft} maxlength="2000" aria-label="Cause"></textarea>
										<div class="row"><button class="btn sm" disabled={saving} onclick={() => saveNote(i.id)}>Save</button><button class="btn sm ghost" onclick={() => (editing = null)}>Cancel</button></div>
									{:else}
										{#if i.note}<span>{i.note.text}</span> <span class="sub">— {i.note.by}</span>{:else}<span class="sub">—</span>{/if}
										{#if admin}<button class="link noprint" onclick={() => { editing = i.id; draft = i.note?.text ?? ''; }}>{i.note ? 'Edit' : 'Add cause'}</button>{/if}
									{/if}
								</td>
							</tr>
						{:else}<tr><td colspan="5" class="empty">No incidents in this period.</td></tr>{/each}
					</tbody>
				</table>
			</div>
		</Panel>
	</div>

	<div class="grid g2 mt">
		<div>
		<Panel title="Certificates" aside="by expiry · now" href={csv('certificates')} hrefLabel="CSV" flush>
			<div class="tbl-wrap flat">
				<table class="tbl">
					<thead><tr><th>Certificate</th><th>Expires</th><th class="r">Left</th></tr></thead>
					<tbody>
						{#each showAllCerts ? r.certs : r.certs.slice(0, 12) as c (c.name + c.where)}
							<tr><td><b>{c.name}</b><div class="sub mono det">{c.where}</div></td><td class="mono nowrap">{dayFmt.format(new Date(c.expires))}</td><td class="r"><Status s={certSev(c.days)} word="{c.days} days" /></td></tr>
						{:else}<tr><td colspan="3" class="empty">No certificates seen yet.</td></tr>{/each}
					</tbody>
				</table>
			</div>
		</Panel>
		{#if r.certs.length > 12}<button class="link noprint" onclick={() => (showAllCerts = !showAllCerts)}>{showAllCerts ? 'Show the soonest 12' : `Show all ${r.certs.length} certificates`}</button>{/if}
		</div>
		<Panel title="Disk space" aside="trend over the period" href={csv('disks')} hrefLabel="CSV" flush>
			<div class="tbl-wrap flat">
				<table class="tbl">
					<thead><tr><th>Disk</th><th class="r">Used</th><th class="r">Per day</th><th class="r">Full</th></tr></thead>
					<tbody>
						{#each r.disks ?? [] as d (d.host + d.mount)}
							<tr>
								<td><b>{d.host}</b> <span class="mono">{d.mount}</span></td>
								<td class="r mono">{Math.round(d.pct)}% <span class="sub">of {bytes(d.total)}</span></td>
								<td class="r mono">{d.per_day ? (d.per_day > 0 ? '+' : '−') + bytes(Math.abs(d.per_day)) : '—'}</td>
								<td class="r">{#if d.projected}<Status s={d.full_days! < 30 ? 'critical' : d.full_days! < 90 ? 'warning' : 'good'} word={d.projected} />{:else}<span class="sub">{d.per_day > 0 ? 'years away' : 'not filling'}</span>{/if}</td>
							</tr>
						{:else}<tr><td colspan="4" class="empty">Hourly samples start once Cairn OBS has run an hour.</td></tr>{/each}
					</tbody>
				</table>
			</div>
		</Panel>
	</div>

	{#if r.trends?.length}
		<div class="grid g3 mt">
			{#each r.trends as t (t.host)}
				<Panel title={t.host} aside="CPU and memory, daily">
					<div class="trend"><span class="sub">CPU avg {Math.round(t.cpu.reduce((a, b) => a + b, 0) / t.cpu.length)}% · peak {Math.round(Math.max(...t.cpu_max))}%</span><Spark data={t.cpu} max={100} label="CPU per day" /></div>
					<div class="trend"><span class="sub">Memory avg {Math.round(t.mem.reduce((a, b) => a + b, 0) / t.mem.length)}%</span><Spark data={t.mem} max={100} label="Memory per day" /></div>
				</Panel>
			{/each}
		</div>
	{/if}

	{#if r.mail}
		<div class="mt">
			<Panel title="Mail" aside="the monitor account's test messages" href={csv('mail')} hrefLabel="CSV">
				<div class="grid g4 mailstats">
					<div class="stat"><div class="lab">Rounds</div><div class="val">{r.mail.rounds}</div><div class="sub">{r.mail.missed} missed</div></div>
					<div class="stat"><div class="lab">Typical</div><div class="val">{r.mail.median_s.toFixed(1)}<small> s</small></div><div class="sub">95% under {r.mail.p95_s.toFixed(1)} s</div></div>
					<div class="stat"><div class="lab">Slowest</div><div class="val">{r.mail.max_s.toFixed(1)}<small> s</small></div></div>
					<div class="stat"><div class="lab">Queue peak</div><div class="val">{r.mail.queue_max}</div></div>
				</div>
				<ul class="list">
					{#each r.mail.listed as l (l.id)}<li><span>{l.name}</span><Status s={l.degraded_s + l.down_s > 0 ? 'warning' : 'good'} word={l.degraded_s + l.down_s > 0 ? `listed ${duration(l.degraded_s + l.down_s)}` : 'never listed'} /></li>{/each}
				</ul>
			</Panel>
		</div>
	{/if}

	<div class="mt">
		<Panel title="Checks" aside={showAllChecks ? 'all' : 'only those with trouble'} href={csv('checks')} hrefLabel="CSV" flush>
			<div class="tbl-wrap flat">
				<table class="tbl">
					<thead><tr><th>Check</th><th class="r">Uptime</th><th class="r">Down</th><th class="r">Degraded</th></tr></thead>
					<tbody>
						{#each checksShown as u (u.id)}<tr><td>{u.name}</td><td class="r"><Status s={pctSev(u)} word={fmtPct(u.pct)} /></td><td class="r mono">{u.down_s ? duration(u.down_s) : '—'}</td><td class="r mono">{u.degraded_s ? duration(u.degraded_s) : '—'}</td></tr>
						{:else}<tr><td colspan="4" class="empty">Every check was up the whole period.</td></tr>{/each}
					</tbody>
				</table>
			</div>
		</Panel>
		<button class="link noprint" onclick={() => (showAllChecks = !showAllChecks)}>{showAllChecks ? 'Show only checks with trouble' : `Show all ${r.checks.length} checks`}</button>
	</div>

	{#if data?.digest}
		{@const dg = data.digest}
		<div class="mt noprint">
			<Panel title="Weekly digest" aside="{dg.weekday}s at {dg.at} · {dg.channels.join(', ')}">
				<p class="sub">A summary of the last seven days goes out every {dg.weekday} at {dg.at} ({dg.timezone}), so a quiet week arrives as an all-clear rather than silence.{#if dg.last}{' '}Last sent {dg.last}.{/if}</p>
				{#if admin}
					<div class="row"><button class="btn sm" onclick={showDigest}>Preview</button>{#if preview && live.admin}<button class="btn sm" onclick={sendDigest}>Send now</button>{/if}<span class="sub">{digestMsg}</span></div>
					{#if preview}<pre class="digest"><b>{preview.subject}</b>{'\n\n'}{preview.text}</pre>{/if}
				{/if}
			</Panel>
		</div>
	{/if}
{:else}
	<p class="empty">Loading…</p>
{/if}

<style>
	.flat { border: 0; border-radius: 0; }
	.stack { display: grid; gap: 20px; align-content: start; }
	.g3 { grid-template-columns: repeat(3, minmax(0, 1fr)); }
	.det { overflow-wrap: anywhere; max-width: 340px; }
	.nowrap { white-space: nowrap; }
	.cause { max-width: 360px; }
	.cause textarea { width: 100%; font: inherit; background: var(--background); color: var(--foreground); border: 1px solid var(--border); border-radius: 6px; padding: 6px; }
	.row { display: flex; gap: 8px; align-items: center; margin-top: 6px; flex-wrap: wrap; }
	.link { background: none; border: 0; color: var(--primary); cursor: pointer; padding: 4px 0; font: inherit; font-size: 13px; }
	.sel { background: var(--card); color: var(--foreground); border: 1px solid var(--border); border-radius: 8px; padding: 6px 10px; font: inherit; }
	.trend + .trend { margin-top: 12px; }
	.list { list-style: none; margin: 14px 0 0; padding: 0; display: grid; gap: 8px; }
	.list li { display: flex; justify-content: space-between; gap: 12px; }
	.mailstats { gap: 12px; }
	.digest { white-space: pre-wrap; font: 12px/1.5 var(--mono); background: var(--background); border: 1px solid var(--border); border-radius: 8px; padding: 12px; margin-top: 12px; max-height: 480px; overflow: auto; }
	.printhead { display: none; }
	:global(.panel) .empty { padding: 20px; }
	@media (max-width: 1100px) { .g3 { grid-template-columns: 1fr; } }
	@media print {
		.noprint { display: none !important; }
		.printhead { display: block; font-weight: 600; margin: 0 0 8px; }
		.grid { grid-template-columns: 1fr !important; }
		:global(.side), :global(header), :global(.topbar) { display: none !important; }
		:global(.layout) { display: block !important; }
		:global(.panel) { break-inside: avoid; }
	}
</style>
