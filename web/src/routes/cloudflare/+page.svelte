<script lang="ts">
	import Icon from '$lib/Icon.svelte';
	import Panel from '$lib/Panel.svelte';
	import Status from '$lib/Status.svelte';
	import { get, post, SignedOut, type Cloudflare, type CfOrigin, type CfTraffic, type Severity } from '$lib/api';
	import { live } from '$lib/live.svelte';
	import { ago, duration, utc } from '$lib/format';

	let data = $state<Cloudflare | null>(null);
	let error = $state('');

	async function load() {
		try {
			data = await get<Cloudflare>('/api/v1/cloudflare');
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

	const DAY = 86400000;
	const days = (t: string) => Math.floor((new Date(t).getTime() - Date.now()) / DAY);
	function origin(o: CfOrigin): [Severity, string] {
		if (!o.enabled) return ['off', 'Off in the pool'];
		return o.healthy ? ['good', 'Healthy'] : ['critical', 'Failing'];
	}
	function certState(c: { status: string; expires?: string }): [Severity, string] {
		if (c.status !== 'active') return ['warning', c.status.replaceAll('_', ' ')];
		if (!c.expires || !data) return ['good', 'Active'];
		const d = days(c.expires);
		const word = `${d} ${d === 1 ? 'day' : 'days'} left`;
		if (d < (data.cert_critical_days ?? 3)) return ['critical', word];
		if (d < (data.cert_warn_days ?? 14)) return ['warning', word];
		return ['good', word];
	}
	function errSev(t: CfTraffic): Severity {
		const pct = t.requests ? (100 * t.errors) / t.requests : 0;
		if (pct >= (data?.error_critical_pct ?? 25) && t.errors >= 20) return 'critical';
		if (pct >= (data?.error_warn_pct ?? 5) && t.errors >= 20) return 'warning';
		return t.errors ? 'off' : 'good';
	}
	let accepting = $state('');
	async function accept(zone: string) {
		accepting = zone;
		try {
			await post(`/api/v1/cloudflare/dns/${encodeURIComponent(zone)}/accept`);
			await load();
		} catch (e) {
			error = String(e);
		} finally {
			accepting = '';
		}
	}
	const changes = $derived(Object.entries(data?.dns_changes ?? {}));
	const statusSev = (i: string): Severity => (i === 'none' ? 'good' : i === 'minor' ? 'warning' : i === 'unknown' ? 'off' : 'serious');
	const snap = $derived(data?.snapshot);
</script>

<svelte:head><title>Cairn OBS · Cloudflare</title></svelte:head>

<div class="phead">
	<span class="tile"><Icon name="cloud" /></span>
	<div>
		<h1>Cloudflare</h1>
		<p>What Cloudflare sees, read with a read-only API token: load balancer health for each origin, every zone's SSL mode and edge certificates, and Cloudflare's own status.</p>
	</div>
</div>

{#if error}<p class="foot-note" style="color:var(--critical)">{error}</p>{/if}

{#if data && !data.configured}
	<Panel title="Not set up">
		<p class="sub">Add a <span class="mono">cloudflare</span> section with a <span class="mono">token_file</span> to obs-beacond.yaml. The token needs read access only: zones, zone settings, DNS, SSL and certificates, analytics, load balancers, and the account's load balancing pools and monitors.</p>
	</Panel>
{:else if snap}
	<div class="meta-row">
		{#if snap.checked && !snap.checked.startsWith('0001')}
			<span class="sub" title={utc(snap.checked)}>Checked {ago(snap.checked)} · every {duration(data!.every_s)}</span>
		{:else}
			<span class="sub">Waiting for the first reading…</span>
		{/if}
		<span class="cfstat"><Status s={statusSev(snap.status.indicator)} word={'Cloudflare status: ' + (snap.status.description || snap.status.indicator)} /></span>
	</div>
	{#if snap.error}
		<p class="foot-note" style="color:var(--warning)">The last reading failed{snap.error_since ? ` (since ${ago(snap.error_since)})` : ''}: {snap.error}. Showing the last good one.</p>
	{/if}

	{#if snap.traffic_error}
		<p class="foot-note" style="color:var(--warning)">Traffic numbers couldn't be read this time: {snap.traffic_error}.</p>
	{/if}

	{#if changes.length}
		<Panel title="DNS changes" aside="{changes.length} {changes.length === 1 ? 'zone' : 'zones'} changed since last accepted">
			{#each changes as [zone, c] (zone)}
				<div class="chg">
					<div class="chg-head"><b>{zone}</b><button class="btn pri sm" hidden={live.session?.role !== 'admin'} disabled={accepting === zone} onclick={() => accept(zone)}>{accepting === zone ? 'Accepting…' : 'Accept'}</button></div>
					{#each c.removed as r (r)}<div class="mono rec del">− {r}</div>{/each}
					{#each c.added as r (r)}<div class="mono rec add">+ {r}</div>{/each}
				</div>
			{/each}
			<p class="sub">If a change wasn't meant, put it back in Cloudflare and the warning clears by itself. Accepting makes today's records the ones to compare against.</p>
		</Panel>
	{/if}

	{#each snap.pools as p (p.name)}
		<Panel title="Pool {p.name}" aside={!p.enabled ? 'disabled' : p.healthy ? 'healthy' : 'no healthy origin'} flush>
			<div class="tbl-wrap flat">
				<table class="tbl">
					<thead><tr><th>Origin</th><th>Address</th><th>Health check</th><th class="r">Answer</th></tr></thead>
					<tbody>
						{#each p.origins as o (o.name + o.address)}
							{@const [s, w] = origin(o)}
							<tr>
								<td>{o.name}</td>
								<td class="mono sub">{o.address}</td>
								<td><Status {s} word={w} />{#if o.enabled && !o.healthy && o.reason}<div class="sub">{o.reason}</div>{/if}</td>
								<td class="r mono">{o.code ? o.code : '—'}{o.rtt ? ' · ' + o.rtt : ''}</td>
							</tr>
						{:else}
							<tr><td colspan="4" class="empty">No origins in this pool.</td></tr>
						{/each}
					</tbody>
				</table>
			</div>
		</Panel>
	{/each}

	<div class="mt">
		<Panel title="Zones" aside="{snap.zones.length} zones · traffic over the last 15 minutes" flush>
			<div class="tbl-wrap flat">
				<table class="tbl">
					<thead><tr><th>Zone</th><th>SSL mode</th><th>Edge certificate</th><th class="r">Requests</th><th class="r">Server errors</th><th class="r">Blocked</th><th class="r">DNS</th></tr></thead>
					<tbody>
						{#each snap.zones as z (z.name)}
							{@const want = data!.ssl_mode_zones?.[z.name] ?? data!.ssl_mode}
							<tr>
								<td>{z.name}</td>
								<td>{#if z.ssl === want}<Status s="good" word={z.ssl} />{:else}<Status s="warning" word={z.ssl || 'unknown'} /><div class="sub">should be {want}</div>{/if}{#if want !== data!.ssl_mode}<div class="sub">expected here, not {data!.ssl_mode}</div>{/if}</td>
								<td>
									{#each z.certs as c, n (n)}
										{@const [s, w] = certState(c)}
										<div title="{c.type} certificate for {c.hosts.join(', ')}"><Status {s} word={w} /></div>
									{:else}
										<Status s="warning" word="None" />
									{/each}
								</td>
								{#if z.traffic}
									<td class="r mono">{z.traffic.requests.toLocaleString()}</td>
									<td class="r">{#if z.traffic.errors}{@const sev = errSev(z.traffic)}{@const word = `${z.traffic.errors} · ${((100 * z.traffic.errors) / z.traffic.requests).toFixed(1)}%`}{#if sev === 'off'}<span class="mono">{word}</span>{:else}<Status s={sev} {word} />{/if}{#if z.traffic.error_hosts[0]}<div class="sub">{z.traffic.error_hosts[0].host}</div>{/if}{:else}<span class="mono sub">0</span>{/if}</td>
									<td class="r mono" title="{z.traffic.blocked_day} in the 24 hours before">{z.traffic.blocked}</td>
								{:else}
									<td class="r sub" colspan="3">—</td>
								{/if}
								<td class="r">{#if data!.dns_changes?.[z.name]}<Status s="warning" word="Changed" />{:else}<span class="mono sub">{z.dns_records}</span>{/if}</td>
							</tr>
						{:else}
							<tr><td colspan="7" class="empty">No zones.</td></tr>
						{/each}
					</tbody>
				</table>
			</div>
		</Panel>
	</div>
	<p class="foot-note">Traffic is the 15 minutes up to a minute ago (Cloudflare's numbers lag a little). Server errors alert at {data!.error_warn_pct}% (critical at {data!.error_critical_pct}%) once there are at least 20; blocked means answered by Cloudflare's own security, not refused by the site. Load balancers per zone: {snap.zones.filter((z) => z.load_balancers.length).map((z) => z.name + ' ' + z.load_balancers.length).join(', ') || 'none'}.</p>
	<p class="foot-note">Cloudflare's status page is shown but never alerts: an incident somewhere in Cloudflare's network usually isn't ours, and when it is, the pools above say so.</p>
{:else}
	<p class="empty">Loading…</p>
{/if}

<style>
	.flat { border: 0; border-radius: 0; }
	.meta-row { display: flex; flex-wrap: wrap; gap: 8px 20px; align-items: center; justify-content: space-between; margin-bottom: 16px; }
	.mt { margin-top: 16px; }
	.chg { padding: 10px 0; border-bottom: 1px solid var(--border); }
	.chg-head { display: flex; align-items: center; justify-content: space-between; gap: 12px; margin-bottom: 6px; }
	.rec { font-size: 13px; overflow-wrap: anywhere; }
	.rec.del { color: var(--muted-fg); text-decoration: line-through; }
	.rec.add { color: var(--foreground); }
	.sm { height: 30px; padding: 0 12px; font-size: 13px; }
	:global(.panel) + :global(.panel) { margin-top: 16px; }
	:global(.panel) .empty { padding: 20px; }
</style>
