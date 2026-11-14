<script lang="ts">
	import Icon from '$lib/Icon.svelte';
	import Panel from '$lib/Panel.svelte';
	import Status from '$lib/Status.svelte';
	import { get, post, SignedOut, type Akamai, type AkActivation, type AkDomain, type AkEnrollment, type AkPMProperty, type AkProperty, type AkServer, type AkTarget, type Severity } from '$lib/api';
	import { live } from '$lib/live.svelte';
	import { ago, duration, utc } from '$lib/format';

	let data = $state<Akamai | null>(null);
	let error = $state('');

	async function load() {
		try {
			data = await get<Akamai>('/api/v1/akamai');
			error = '';
		} catch (e) {
			if (e instanceof SignedOut) void live.checkSession();
			else error = String(e);
		}
	}
	$effect(() => {
		void load();
		const t = setInterval(() => void load(), 30000);
		return () => clearInterval(t);
	});

	const snap = $derived(data?.snapshot);
	const live_ = (t: AkTarget) => t.servers.some((s) => s.alive);
	function property(p: AkProperty): [Severity, string] {
		if (p.error) return ['warning', 'Not read'];
		if (!p.at || p.at.startsWith('0001')) return ['off', 'No report yet'];
		const on = p.targets.filter((t) => t.enabled && t.servers.length);
		const up = on.filter(live_).length;
		if (!on.length) return ['off', 'No datacenter enabled'];
		if (!up) return ['critical', 'No live datacenter'];
		if (up < on.length) return ['warning', `${up} of ${on.length} datacenters live`];
		return ['good', `${up} ${up === 1 ? 'datacenter' : 'datacenters'} live`];
	}
	function server(t: AkTarget, s: AkServer): [Severity, string] {
		if (!t.enabled) return ['off', 'Disabled'];
		if (!s.known) return ['off', 'Not in the report'];
		if (!s.alive) return ['critical', 'Failing'];
		return s.handed_out ? ['good', 'Alive, handed out'] : ['good', 'Alive'];
	}
	function propagation(p?: string): [Severity, string] {
		switch (p) {
			case 'COMPLETE': return ['good', 'Propagated'];
			case 'PENDING': return ['warning', 'Propagating'];
			case 'DENIED': return ['warning', 'Refused'];
		}
		return ['off', p || 'Unknown'];
	}
	const waitingFor: Record<string, string> = {
		'third-party-csr': 'the CSR signed by your certificate authority',
		'third-party-certificate': 'the certificate your authority issued',
		'change-management-info': 'the staging certificate acknowledged',
		'lets-encrypt-challenges': 'domain validation',
		'pre-verification-warnings-acknowledgement': 'warnings acknowledged',
		'post-verification-warnings-acknowledgement': 'warnings acknowledged',
	};
	function change(e: AkEnrollment): [Severity, string] {
		if (e.waiting) return ['warning', 'Waiting on you: ' + (waitingFor[e.waiting] ?? e.waiting)];
		if (e.pending) return ['off', e.pending === 1 ? 'A change in progress' : `${e.pending} changes in progress`];
		return ['good', 'None pending'];
	}
	const validation: Record<string, string> = { dv: 'DV', ov: 'OV', ev: 'EV', 'third-party': 'Third-party' };

	// Property Manager: problems first, then by name; the first 50 unless asked.
	const pending = ['NEW', 'PENDING', 'ZONE_1', 'ZONE_2', 'ZONE_3', 'PENDING_DEACTIVATION', 'PENDING_CANCELLATION'];
	function activation(a?: AkActivation): [Severity, string] {
		if (!a) return ['off', 'Never activated'];
		const v = `v${a.version}`;
		const verb = a.type === 'DEACTIVATE' ? 'Deactivating' : 'Activating';
		if (a.status === 'FAILED') return ['warning', `${verb} ${v} failed`];
		if (pending.includes(a.status)) {
			const late = Date.now() - Date.parse(a.submitted) > 3600_000;
			return [late ? 'warning' : 'off', `${verb} ${v}: ${a.status.toLowerCase().replace('_', ' ')}`];
		}
		if (a.status === 'ACTIVE') return ['good', `${v} active`];
		return ['off', `${v} ${a.status.toLowerCase()}`];
	}
	const rank = (p: AkPMProperty) => Math.max(...[p.last_staging, p.last_production].map((a) => ({ critical: 2, warning: 1 } as Record<string, number>)[activation(a)[0]] ?? 0), p.error ? 1 : 0);
	const props = $derived([...(snap?.pm_properties ?? [])].sort((a, b) => rank(b) - rank(a) || a.name.localeCompare(b.name)));
	let allProps = $state(false);

	// Edge DNS: changes since last accepted, as Cloudflare's.
	let accepting = $state('');
	async function accept(zone: string) {
		accepting = zone;
		try {
			await post(`/api/v1/akamai/dns/${encodeURIComponent(zone)}/accept`);
			await load();
		} catch (e) {
			error = String(e);
		} finally {
			accepting = '';
		}
	}
	const changes = $derived(Object.entries(data?.dns_changes ?? {}));

	// Traffic: busiest first; the first 20 unless asked, but every failing one.
	const statusSev = (i: string): Severity => (i === 'none' ? 'good' : i === 'minor' ? 'warning' : i === 'unknown' ? 'off' : 'serious');
	function errSev(t: { hits: number; errors: number }): Severity {
		const pct = t.hits ? (100 * t.errors) / t.hits : 0;
		if (t.errors < (data?.error_min ?? 20)) return t.errors ? 'off' : 'good';
		if (pct >= (data?.error_critical_pct ?? 25)) return 'critical';
		if (pct >= (data?.error_warn_pct ?? 5)) return 'warning';
		return 'off';
	}
	const traffic = $derived(snap?.traffic ?? []);
	let allTraffic = $state(false);
	const shownTraffic = $derived(allTraffic ? traffic : traffic.filter((t, i) => i < 20 || errSev(t) === 'warning' || errSev(t) === 'critical'));
	const pct = (t: { hits: number; errors: number }) => (t.hits ? ((100 * t.errors) / t.hits).toFixed(t.errors && (100 * t.errors) / t.hits < 1 ? 2 : 1) + '%' : '—');
	const zones = $derived(snap?.dns_zones ?? []);
	const records = $derived(zones.reduce((n, z) => n + z.count, 0));
	const shownProps = $derived(allProps ? props : props.slice(0, 50));

	// At enterprise scale the page is long: a line of sections with what
	// needs a look in each, and GTM domains folded unless something in
	// them does (or there are only a couple).
	const bad = (s: Severity) => s === 'warning' || s === 'critical' || s === 'serious';
	const worse = (a: Severity, b: Severity): Severity => {
		const r: Record<string, number> = { critical: 4, serious: 3, warning: 2, off: 1, good: 0 };
		return (r[b] ?? 0) > (r[a] ?? 0) ? b : a;
	};
	function domainSev(d: AkDomain): [Severity, string] {
		let sev: Severity = d.error ? 'warning' : 'good';
		const ps = propagation(d.propagation)[0];
		if (bad(ps)) sev = worse(sev, ps);
		const sick = d.properties.filter((p) => bad(property(p)[0]));
		for (const p of sick) sev = worse(sev, property(p)[0]);
		const n = d.properties.length;
		const word = d.error ? "Couldn't read it" : sick.length ? `${sick.length} of ${n} ${n === 1 ? 'property' : 'properties'} ${sick.length === 1 ? 'needs' : 'need'} a look` : bad(ps) ? propagation(d.propagation)[1] : `${n} ${n === 1 ? 'property' : 'properties'}, all well`;
		return [sev, word];
	}
	const domains = $derived(snap?.domains ?? []);
	// Inside a domain, what needs a look comes first.
	const sickFirst = (ps: AkProperty[]) => [...ps].sort((a, b) => Number(bad(property(b)[0])) - Number(bad(property(a)[0])));
	let folded = $state<Record<string, boolean>>({});
	const isOpen = (d: AkDomain) => (folded[d.name] === undefined ? domains.length <= 2 || bad(domainSev(d)[0]) : !folded[d.name]);
	const toggle = (d: AkDomain) => (folded = { ...folded, [d.name]: isOpen(d) });
	const setAll = (open: boolean) => (folded = Object.fromEntries(domains.map((d) => [d.name, !open])));
	const sections = $derived([
		{ id: 'gtm', name: 'GTM', n: domains.length, bad: domains.filter((d) => bad(domainSev(d)[0])).length },
		{ id: 'traffic', name: 'Traffic', n: traffic.length, bad: traffic.filter((t) => bad(errSev(t))).length },
		{ id: 'certificates', name: 'Certificates', n: snap?.enrollments?.length ?? 0, bad: (snap?.enrollments ?? []).filter((e) => e.waiting || e.deployed.some((c) => bad(data?.certs?.[c.fingerprint]?.status ?? 'good'))).length },
		{ id: 'properties', name: 'Properties', n: props.length, bad: props.filter((p) => rank(p) > 0).length },
		{ id: 'dns', name: 'Edge DNS', n: zones.length, bad: changes.length + zones.filter((z) => z.error).length },
	].filter((x) => x.n > 0));
</script>

<svelte:head><title>Cairn OBS · Akamai</title></svelte:head>

<div class="phead">
	<span class="tile"><Icon name="globe" /></span>
	<div>
		<h1>Akamai</h1>
		<p>What Akamai sees, read with a read-only API client: Global Traffic Management domains, each property's datacenters, and whether GTM's liveness tests find their servers alive; the certificates CPS has on Akamai's production network; each Property Manager property's newest activations; Edge DNS changes; each CP code's share of 5xx; and Akamai's own status.</p>
	</div>
</div>

{#if error}<p class="foot-note" style="color:var(--critical)">{error}</p>{/if}

{#if data && !data.configured}
	<Panel title="Not set up">
		<p class="sub">Add an <span class="mono">akamai</span> section with an <span class="mono">edgerc_file</span> to obs-beacond.yaml. Make the API client in Akamai Control Center (Identity and Access, API clients) with read-only grants on Global Traffic Management and its reporting, on the Certificate Provisioning System, Property Manager, Edge DNS and Reporting.</p>
	</Panel>
{:else if snap}
	<div class="meta-row">
		{#if snap.checked && !snap.checked.startsWith('0001')}
			<span class="sub" title={utc(snap.checked)}>Checked {ago(snap.checked)} · every {duration(data!.every_s)}</span>
		{:else}
			<span class="sub">Waiting for the first reading…</span>
		{/if}
		{#if snap.status}<span><Status s={statusSev(snap.status.indicator)} word={'Akamai status: ' + (snap.status.description || snap.status.indicator)} /></span>{/if}
	</div>
	{#if snap.error}
		<p class="foot-note" style="color:var(--warning)">The last reading failed{snap.error_since ? ` (since ${ago(snap.error_since)})` : ''}: {snap.error}. Showing the last good one.</p>
	{/if}
	{#if sections.length > 1}
		<nav class="sections" aria-label="Sections">
			{#each sections as x (x.id)}
				<a class="chip" href="#{x.id}">{x.name} <b>{x.n.toLocaleString()}</b>{#if x.bad}<span class="need">{x.bad} {x.bad === 1 ? 'needs' : 'need'} a look</span>{/if}</a>
			{/each}
		</nav>
	{/if}
	{#if snap.absent?.includes('gtm')}
		<p class="foot-note">Global Traffic Management isn't in this account, or the API client isn't granted it.</p>
	{/if}

	{#if domains.length}
		<div id="gtm" class="anchor"></div>
		<Panel title="GTM domains" aside="{domains.length} {domains.length === 1 ? 'domain' : 'domains'} · {domains.reduce((n, d) => n + d.properties.length, 0).toLocaleString()} properties" flush>
			{#if domains.length > 2}
				<div class="dom-tools">
					<span class="sub">Domains with something to look at are open.</span>
					<span><button class="btn sm" onclick={() => setAll(true)}>Open all</button> <button class="btn sm" onclick={() => setAll(false)}>Fold all</button></span>
				</div>
			{/if}
	{#each domains as d (d.name)}
		{@const [ps, pw] = propagation(d.propagation)}
		{@const [ds, dw] = domainSev(d)}
		<div class="dom">
			<button class="dom-head" aria-expanded={isOpen(d)} onclick={() => toggle(d)}>
				<span class="caret" class:open={isOpen(d)}>▸</span>
				<span class="mono nm">{d.name}</span>
				<span class="sub">{d.type ?? ''}</span>
				<span class="grow"></span>
				<Status s={ds} word={dw} />
			</button>
		{#if isOpen(d)}
			<div class="dom-meta">
				<Status s={ps} word={pw} />
				{#if d.propagation === 'PENDING' && d.since && !d.since.startsWith('0001')}<span class="sub">since {ago(d.since)}</span>{/if}
				{#if d.propagation === 'DENIED' && d.message}<span class="sub">{d.message}</span>{/if}
				{#if d.error}<span class="sub" style="color:var(--warning)">Couldn't read it: {d.error}</span>{/if}
			</div>
			<div class="tbl-wrap flat">
				<table class="tbl">
					<thead><tr><th>Property</th><th>Datacenter</th><th>Server</th><th>Liveness</th><th class="r">Score</th></tr></thead>
					<tbody>
						{#each sickFirst(d.properties) as p (p.name)}
							{@const [s, w] = property(p)}
							<tr class="prop">
								<td><span class="mono">{p.name}.{d.name}</span>{#if p.type}<div class="sub">{p.type}</div>{/if}</td>
								<td colspan="3"><Status {s} word={w} />{#if p.error}<div class="sub">{p.error}</div>{/if}</td>
								<td class="r sub">{p.at && !p.at.startsWith('0001') ? ago(p.at) : ''}</td>
							</tr>
							{#each p.targets as t (t.id)}
								{#each t.servers as sv, i (sv.ip)}
									{@const [ss, sw] = server(t, sv)}
									<tr>
										<td></td>
										<td>{#if i === 0}{t.datacenter}{#if !t.enabled}<div class="sub">disabled</div>{/if}{/if}</td>
										<td class="mono">{sv.ip}{#if data?.hosts?.[sv.ip]}<div><a class="sub" href="/hosts/{data.hosts[sv.ip]}">{data.hosts[sv.ip]}</a></div>{/if}</td>
										<td><Status s={ss} word={sw} /></td>
										<td class="r mono">{sv.known ? sv.score : '—'}</td>
									</tr>
								{/each}
							{/each}
						{:else}
							<tr><td colspan="5" class="empty">No properties.</td></tr>
						{/each}
					</tbody>
				</table>
			</div>
		{/if}
		</div>
	{/each}
		</Panel>
	{:else if !snap.absent?.length}
		<p class="empty">No GTM domains.</p>
	{/if}
	{#if traffic.length}
		<div id="traffic" class="anchor"></div>
		<Panel title="Traffic" aside="{traffic.length} CP {traffic.length === 1 ? 'code' : 'codes'}{snap.traffic_end && !snap.traffic_end.startsWith('0001') ? ' · ' + utc(snap.traffic_start ?? '').slice(11, 16) + '–' + utc(snap.traffic_end).slice(11, 16) + ' UTC' : ''}" flush>
			<div class="tbl-wrap flat">
				<table class="tbl">
					<thead><tr><th>CP code</th><th class="r">Edge hits</th><th class="r">5xx</th><th>Share</th></tr></thead>
					<tbody>
						{#each shownTraffic as t (t.cpcode)}
							{@const s_ = errSev(t)}
							<tr>
								<td class="mono">{t.cpcode}</td>
								<td class="r mono">{t.hits.toLocaleString()}</td>
								<td class="r mono">{t.errors.toLocaleString()}</td>
								<td><Status s={s_} word={pct(t)} /></td>
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
			{#if shownTraffic.length < traffic.length || allTraffic}
				<div class="more"><button class="btn" onclick={() => (allTraffic = !allTraffic)}>{allTraffic ? 'Show the busiest' : `Show all ${traffic.length}`}</button></div>
			{/if}
		</Panel>
	{/if}
	{#if snap.traffic_error}
		<p class="foot-note" style="color:var(--warning)">Traffic couldn't be read this time: {snap.traffic_error}.{traffic.length ? ' Showing the last numbers.' : ''}</p>
	{/if}
	{#if snap.absent?.includes('traffic')}
		<p class="foot-note">Traffic reports aren't in this account, or the API client isn't granted the Reporting API.</p>
	{/if}
	{#if snap.enrollments?.length}
		<div id="certificates" class="anchor"></div>
		<Panel title="Certificates" aside="{snap.enrollments.length} CPS {snap.enrollments.length === 1 ? 'enrollment' : 'enrollments'}{snap.cps_at && !snap.cps_at.startsWith('0001') ? ' · read ' + ago(snap.cps_at) : ''}" flush>
			<div class="tbl-wrap flat">
				<table class="tbl">
					<thead><tr><th>Enrollment</th><th>Deployed certificate</th><th>Key</th><th>Issuer</th><th>Change</th></tr></thead>
					<tbody>
						{#each snap.enrollments as e (e.id)}
							{@const [cs, cw] = change(e)}
							{@const n = Math.max(e.deployed.length, 1)}
							{#each { length: n } as _, i}
								{@const c = e.deployed[i]}
								{@const j = c ? data?.certs?.[c.fingerprint] : undefined}
								<tr class:prop={i === 0}>
									<td>{#if i === 0}<span class="mono">{e.cn}</span><div class="sub">#{e.id}{e.validation ? ' · ' + (validation[e.validation] ?? e.validation) : ''}{e.sans && e.sans.length > 1 ? ` · ${e.sans.length} names` : ''}</div>{/if}</td>
									<td>
										{#if c && j}<Status s={j.status} word={j.word} />{#if j.weak}<div class="sub" style="color:var(--warning)">Weak: {j.weak}</div>{/if}
										{:else if e.error}<span class="sub" style="color:var(--warning)">Couldn't read it: {e.error}</span>
										{:else}<span class="sub">Not on production yet</span>{/if}
									</td>
									<td class="mono" style="white-space:nowrap">{c?.key ?? ''}</td>
									<td class="sub">{c?.issuer ?? ''}</td>
									<td>{#if i === 0}<Status s={cs} word={cw} />{/if}</td>
								</tr>
							{/each}
						{/each}
					</tbody>
				</table>
			</div>
		</Panel>
	{/if}
	{#if props.length}
		<div id="properties" class="anchor"></div>
		<Panel title="Properties" aside="{props.length} in Property Manager{snap.papi_at && !snap.papi_at.startsWith('0001') ? ' · read ' + ago(snap.papi_at) : ''}" flush>
			<div class="tbl-wrap flat">
				<table class="tbl">
					<thead><tr><th>Property</th><th class="r">Latest</th><th>Production</th><th>Staging</th></tr></thead>
					<tbody>
						{#each shownProps as p (p.id)}
							{@const [ps_, pw_] = activation(p.last_production)}
							{@const [ss_, sw_] = activation(p.last_staging)}
							<tr>
								<td><span class="mono">{p.name}</span><div class="sub">{p.group}</div>{#if p.error}<div class="sub" style="color:var(--warning)">Couldn't read its activations: {p.error}</div>{/if}</td>
								<td class="r mono">v{p.latest}</td>
								<td><Status s={ps_} word={pw_} />{#if p.last_production}<div class="sub" title={utc(p.last_production.submitted)}>{ago(p.last_production.submitted)}{p.last_production.status !== 'ACTIVE' && p.production ? ` · v${p.production} active` : ''}{p.last_production.note ? ' · ' + p.last_production.note : ''}</div>{/if}</td>
								<td><Status s={ss_} word={sw_} />{#if p.last_staging}<div class="sub" title={utc(p.last_staging.submitted)}>{ago(p.last_staging.submitted)}{p.last_staging.status !== 'ACTIVE' && p.staging ? ` · v${p.staging} active` : ''}{p.last_staging.note ? ' · ' + p.last_staging.note : ''}</div>{/if}</td>
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
			{#if props.length > 50}
				<div class="more"><button class="btn" onclick={() => (allProps = !allProps)}>{allProps ? 'Show the first 50' : `Show all ${props.length}`}</button></div>
			{/if}
		</Panel>
	{/if}
	{#if zones.length || changes.length}<div id="dns" class="anchor"></div>{/if}
	{#if changes.length}
		<Panel title="Edge DNS changes" aside="{changes.length} {changes.length === 1 ? 'zone' : 'zones'} changed since last accepted">
			{#each changes as [zone, c] (zone)}
				<div class="chg">
					<div class="chg-head"><b>{zone}</b><button class="btn pri sm" hidden={live.session?.role !== 'admin'} disabled={accepting === zone} onclick={() => accept(zone)}>{accepting === zone ? 'Accepting…' : 'Accept'}</button></div>
					{#each c.removed as r (r)}<div class="mono rec del">− {r}</div>{/each}
					{#each c.added as r (r)}<div class="mono rec add">+ {r}</div>{/each}
				</div>
			{/each}
			<p class="sub">If a change wasn't meant, put it back in Edge DNS and the warning clears by itself. Accepting makes today's records the ones to compare against.</p>
		</Panel>
	{/if}
	{#if zones.length}
		<p class="foot-note">Edge DNS: {zones.length} {zones.length === 1 ? 'zone' : 'zones'}, {records.toLocaleString()} record sets watched for changes{snap.dns_at && !snap.dns_at.startsWith('0001') ? ', read ' + ago(snap.dns_at) : ''}.{#each zones.filter((z) => z.error) as z (z.name)} {z.name} couldn't be read: {z.error}.{/each}</p>
	{/if}
	{#if snap.dns_error}
		<p class="foot-note" style="color:var(--warning)">The last reading of Edge DNS failed: {snap.dns_error}.{zones.length ? ' Showing the one before.' : ''}</p>
	{/if}
	{#if snap.absent?.includes('dns')}
		<p class="foot-note">Edge DNS isn't in this account, or the API client isn't granted it.</p>
	{/if}
	{#if snap.papi_error}
		<p class="foot-note" style="color:var(--warning)">The last reading of Property Manager failed: {snap.papi_error}.{props.length ? ' Showing the one before.' : ''}</p>
	{/if}
	{#if snap.absent?.includes('papi')}
		<p class="foot-note">Property Manager isn't in this account, or the API client isn't granted it.</p>
	{/if}
	{#if snap.cps_error}
		<p class="foot-note" style="color:var(--warning)">The last reading of CPS failed: {snap.cps_error}.{snap.enrollments?.length ? ' Showing the one before.' : ''}</p>
	{/if}
	{#if snap.absent?.includes('cps')}
		<p class="foot-note">The Certificate Provisioning System isn't in this account, or the API client isn't granted it.</p>
	{/if}
	<p class="foot-note">Liveness is GTM's most recent IP availability report. A datacenter with no live server is skipped by GTM; a property with none alerts as critical. A server whose address a host has links to that host, and its maintenance window holds the alert. CPS certificates are also on the Certificates page; their alerts are here, at 21 and 7 days, and when a renewal waits on you. A property's activation alerts when it failed, or has been pending over an hour; an Edge DNS zone when its records change, until the change is undone or accepted. Traffic is Akamai's latest 5-minute report, about ten minutes behind; a CP code alerts at {data?.error_warn_pct ?? 5}% and {data?.error_critical_pct ?? 25}% 5xx with at least {data?.error_min ?? 20}. Akamai's status is shown, never alerting.</p>
{:else}
	<p class="empty">Loading…</p>
{/if}

<style>
	.flat { border: 0; border-radius: 0; }
	.sections { display: flex; flex-wrap: wrap; gap: 8px; margin: -4px 0 16px; }
	.sections .chip { text-decoration: none; display: inline-flex; gap: 6px; align-items: center; }
	.need { color: var(--warning); font-weight: 600; }
	.anchor { scroll-margin-top: 72px; }
	.dom-tools { display: flex; flex-wrap: wrap; gap: 8px 16px; align-items: center; justify-content: space-between; padding: 0 20px 12px; }
	.dom { border-top: 1px solid var(--border); }
	.dom-head { all: unset; box-sizing: border-box; display: flex; flex-wrap: wrap; align-items: center; gap: 6px 10px; width: 100%; padding: 12px 20px; cursor: pointer; }
	.dom-head:hover { background: var(--accent); }
	.dom-head:focus-visible { outline: 2px solid var(--ring, var(--primary)); outline-offset: -2px; }
	.caret { display: inline-block; transition: transform .15s; color: var(--muted-fg); }
	.caret.open { transform: rotate(90deg); }
	.nm { font-weight: 600; }
	.grow { flex: 1; }
	.meta-row { display: flex; flex-wrap: wrap; gap: 8px 20px; align-items: center; justify-content: space-between; margin-bottom: 16px; }
	.dom-meta { display: flex; flex-wrap: wrap; gap: 6px 14px; align-items: center; padding: 10px 16px; border-bottom: 1px solid var(--border); }
	.prop td { border-top: 1px solid var(--border); }
	.more { padding: 12px 16px; border-top: 1px solid var(--border); }
	.chg { padding: 10px 0; border-bottom: 1px solid var(--border); }
	.chg-head { display: flex; align-items: center; justify-content: space-between; gap: 12px; margin-bottom: 6px; }
	.rec { font-size: 13px; overflow-wrap: anywhere; }
	.rec.del { color: var(--muted-fg); text-decoration: line-through; }
	.rec.add { color: var(--foreground); }
	.sm { height: 30px; padding: 0 12px; font-size: 13px; }
	:global(.panel) + :global(.panel) { margin-top: 16px; }
	:global(.panel) .empty { padding: 20px; }
</style>
