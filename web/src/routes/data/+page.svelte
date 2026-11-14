<script lang="ts">
	import Icon from '$lib/Icon.svelte';
	import Panel from '$lib/Panel.svelte';
	import Status from '$lib/Status.svelte';
	import { get, SignedOut, type Severity } from '$lib/api';
	import { live } from '$lib/live.svelte';
	import { bytes } from '$lib/format';

	type Member = { name: string; role: string; state: string; timeline: number; lag_bytes?: number; dcs_age_s?: number; status: Severity; word: string };
	type Backend = { name: string; up: string[]; down: string[]; rule: string; status: Severity; word: string };
	type Probe = { id: string; kind: string; host: string; status: Severity; word: string; backends?: Backend[]; facts?: Record<string, any> };
	type View = { configured: boolean; postgres?: { configured: boolean; members?: Member[]; leader?: string; reachable?: boolean }; probes?: Probe[]; lag_warn_mb?: number };

	let data = $state<View | null>(null);
	let error = $state('');
	async function load() {
		try {
			data = await get<View>('/api/v1/datalayer');
			error = '';
		} catch (e) {
			if (e instanceof SignedOut) void live.checkSession();
			else error = String(e);
		}
	}
	$effect(() => {
		void load();
		const t = setInterval(() => void load(), 15000);
		return () => clearInterval(t);
	});

	const of = (kind: string) => (data?.probes ?? []).filter((p) => p.kind === kind);
	const ROLE: Record<string, string> = { leader: 'Leader', sync_standby: 'Sync standby', replica: 'Replica', standby_leader: 'Standby leader' };
	const haHosts = $derived(of('haproxy'));
	const backendNames = $derived([...new Set(haHosts.flatMap((p) => (p.backends ?? []).map((b) => b.name)))]);
	const fmtN = (n: number) => (n >= 1e6 ? (n / 1e6).toFixed(1) + 'M' : n >= 1e3 ? (n / 1e3).toFixed(1) + 'k' : String(n));
</script>

<svelte:head><title>Cairn OBS · Data layer</title></svelte:head>

<div class="phead">
	<span class="tile"><Icon name="database" /></span>
	<div>
		<h1>Data layer</h1>
		<p>What the mail cluster stands on: PostgreSQL under Patroni, etcd, HAProxy in front of the database and blob store, NATS and Garage. Read from each service's own status page.</p>
	</div>
</div>

{#if error}<p class="foot-note" style="color:var(--critical)">{error}</p>{/if}

{#if data && !data.configured}
	<Panel title="Not set up"><p class="sub">Add a <span class="mono">datalayer</span> section to obs-beacond.yaml. See docs/formats.md.</p></Panel>
{:else if data}
	{#if data.postgres?.configured}
		<Panel title="PostgreSQL" aside={data.postgres.reachable ? (data.postgres.leader ? `leader ${data.postgres.leader}` : 'no leader') : 'cluster API not reachable'} flush>
			<div class="tbl-wrap flat">
				<table class="tbl">
					<thead><tr><th>Member</th><th>Role</th><th>State</th><th class="r">Timeline</th><th class="r">Behind the leader</th><th class="r">Heard from etcd</th></tr></thead>
					<tbody>
						{#each data.postgres.members ?? [] as m (m.name)}
							<tr>
								<td><b>{m.name}</b></td>
								<td>{#if m.role === 'leader'}<span class="lead">Leader</span>{:else}{ROLE[m.role] ?? m.role}{/if}</td>
								<td><Status s={m.status} word={m.status === 'good' ? m.state : m.word} /></td>
								<td class="r mono">{m.timeline}</td>
								<td class="r mono">{m.role === 'leader' ? '—' : m.lag_bytes == null ? 'unknown' : bytes(m.lag_bytes)}</td>
								<td class="r mono">{m.dcs_age_s == null ? '—' : m.dcs_age_s + ' s ago'}</td>
							</tr>
						{:else}
							<tr><td colspan="6" class="empty">No member has answered yet.</td></tr>
						{/each}
					</tbody>
				</table>
			</div>
		</Panel>
		<p class="foot-note">A replica more than {data.lag_warn_mb} MB behind warns; no leader is critical. "Heard from etcd" is Patroni's own view of etcd.</p>
	{/if}

	{#if haHosts.length}
		<div class="mt">
			<Panel title="HAProxy" aside="per host · judged by what each backend is for" flush>
				<div class="tbl-wrap flat">
					<table class="tbl">
						<thead><tr><th>Backend</th><th>Rule</th>{#each haHosts as p (p.host)}<th>{p.host}</th>{/each}</tr></thead>
						<tbody>
							{#each backendNames as name (name)}
								<tr>
									<td class="mono">{name}</td>
									<td class="sub">{haHosts[0].backends?.find((b) => b.name === name)?.rule ?? ''} up</td>
									{#each haHosts as p (p.host)}
										{@const b = p.backends?.find((x) => x.name === name)}
										<td>{#if b}<Status s={b.status} word={b.status === 'good' ? `${b.up.join(', ') || 'none'} up` : b.word} />{:else}<Status s={p.status} word={p.word} />{/if}</td>
									{/each}
								</tr>
							{/each}
						</tbody>
					</table>
				</div>
			</Panel>
		</div>
	{/if}

	{#if of('etcd').length}
		<div class="mt">
			<Panel title="etcd" aside="who leads PostgreSQL is decided here" flush>
				<div class="tbl-wrap flat">
					<table class="tbl">
						<thead><tr><th>Member</th><th>State</th><th class="r">Leader changes</th><th class="r">Database</th><th class="r">Disk sync (p99)</th></tr></thead>
						<tbody>
							{#each of('etcd') as p (p.host)}
								<tr>
									<td><b>{p.host}</b>{#if p.facts?.is_leader}<span class="lead"> · leader</span>{/if}</td>
									<td><Status s={p.status} word={p.status === 'good' ? (p.facts?.has_leader ? 'Has a leader' : 'OK') : p.word} /></td>
									<td class="r mono">{p.facts?.leader_changes ?? '—'}</td>
									<td class="r mono">{p.facts ? `${bytes(p.facts.db_bytes)} of ${bytes(p.facts.quota_bytes)}` : '—'}</td>
									<td class="r mono">{p.facts?.fsync_p99_ms != null ? (p.facts.fsync_p99_ms >= 1e6 ? 'over 8 s' : Math.round(p.facts.fsync_p99_ms) + ' ms') : 'measuring…'}</td>
								</tr>
							{/each}
						</tbody>
					</table>
				</div>
			</Panel>
		</div>
	{/if}

	<div class="grid g2 mt">
		{#if of('nats').length}
			<Panel title="NATS" aside="routes between the servers" flush>
				<div class="tbl-wrap flat">
					<table class="tbl">
						<thead><tr><th>Host</th><th>State</th><th class="r">Peers</th><th class="r">Messages in / out</th><th class="r">Slow consumers</th></tr></thead>
						<tbody>
							{#each of('nats') as p (p.host)}
								<tr>
									<td><b>{p.host}</b>{#if p.facts?.version}<div class="sub">v{p.facts.version} · up {p.facts.uptime}</div>{/if}</td>
									<td><Status s={p.status} word={p.word} /></td>
									<td class="r mono">{p.facts ? `${p.facts.peers} of ${p.facts.want_peers || '?'}` : '—'}</td>
									<td class="r mono">{p.facts ? `${fmtN(p.facts.in_msgs)} / ${fmtN(p.facts.out_msgs)}` : '—'}</td>
									<td class="r mono">{p.facts?.slow_consumers ?? '—'}</td>
								</tr>
							{/each}
						</tbody>
					</table>
				</div>
			</Panel>
		{/if}
		{#if of('garage').length}
			<Panel title="Garage" aside="blob store" flush>
				<div class="tbl-wrap flat">
					<table class="tbl">
						<thead><tr><th>Host</th><th>State</th><th>Garage says</th></tr></thead>
						<tbody>
							{#each of('garage') as p (p.host)}
								<tr><td><b>{p.host}</b></td><td><Status s={p.status} word={p.status === 'good' ? 'OK' : 'Trouble'} /></td><td class="sub">{p.facts?.says ?? p.word}</td></tr>
							{/each}
						</tbody>
					</table>
				</div>
			</Panel>
		{/if}
	</div>
{:else}
	<p class="empty">Loading…</p>
{/if}

<style>
	.flat { border: 0; border-radius: 0; }
	.lead { color: var(--primary); font-weight: 600; }
	:global(.panel) .empty { padding: 20px; }
</style>
