<script lang="ts">
	import Icon from '$lib/Icon.svelte';
	import Panel from '$lib/Panel.svelte';
	import Status from '$lib/Status.svelte';
	import { get, SignedOut, type Severity } from '$lib/api';
	import { live } from '$lib/live.svelte';
	import { ago, bytes, hmz } from '$lib/format';

	type Dir = { name: string; from?: string; path: string; size: number; modified?: string; prev_size: number; count: number };
	type Scan = {
		at: string; received: Dir[]; made: Dir[]; roots: { path: string; used_pct: number }[];
		jobs: { unit: string; result: string; active: string; finished?: string }[];
		raid: { dev: string; state: string }[]; smart: { disk: string; ok: boolean; error?: string }[]; errors?: string[];
	};
	type Finding = { key: string; severity: Severity; title: string; detail: string };
	type Expect = { received: { from: string; root: string; expect: string[]; max_age_ns: number }[]; made: { name: string; paths: string[]; max_age_ns: number }[]; jobs: string[] };
	type HostView = { host: string; silent: boolean; expect: Expect; scan: Scan | null; findings: Finding[] };
	type View = { configured: boolean; hosts?: HostView[]; max_age_h?: number; shrink?: number; disk_warn?: number; scan_every_s?: number };

	let data = $state<View | null>(null);
	let error = $state('');
	async function load() {
		try {
			data = await get<View>('/api/v1/backups');
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

	const HOUR = 3.6e12; // ns
	function rows(h: HostView) {
		const out: { key: string; name: string; from: string; d?: Dir; maxH: number }[] = [];
		for (const s of h.expect.received ?? [])
			for (const name of s.expect) out.push({ key: `received/${s.from}/${name}`, name, from: s.from, d: h.scan?.received.find((x) => x.name === name && x.from === s.from), maxH: s.max_age_ns / HOUR });
		for (const m of h.expect.made ?? []) out.push({ key: `made/${m.name}`, name: m.name, from: 'the leader', d: h.scan?.made.find((x) => x.name === m.name), maxH: m.max_age_ns / HOUR });
		return out;
	}
	function finding(h: HostView, key: string): Finding | undefined {
		return h.findings.find((f) => f.key === `backup/${h.host}/${key}`);
	}
	function rowState(h: HostView, key: string, d?: Dir): [Severity, string] {
		const f = finding(h, key);
		if (f) return [f.severity, f.severity === 'warning' ? 'Shrank' : !d?.count ? 'Missing' : d.size === 0 ? 'Empty' : 'Late'];
		if (!h.scan) return ['waiting', 'No scan'];
		return ['good', 'Current'];
	}
	function change(d?: Dir): string {
		if (!d?.count || !d.prev_size) return '';
		const pct = ((d.size - d.prev_size) / d.prev_size) * 100;
		if (Math.abs(pct) < 0.5) return 'same';
		return (pct > 0 ? '+' : '') + pct.toFixed(pct > -10 && pct < 10 ? 1 : 0) + '%';
	}
	const base = (p: string) => p.split('/').pop();
	const total = $derived((data?.hosts ?? []).reduce((n, h) => n + rows(h).length, 0));
	const bad = $derived((data?.hosts ?? []).reduce((n, h) => n + h.findings.length, 0));
	const jobSev = (j: { result: string }): Severity => (j.result === 'success' ? 'good' : j.result ? 'serious' : 'warning');
</script>

<svelte:head><title>Cairn OBS · Backups</title></svelte:head>

<div class="phead">
	<span class="tile"><Icon name="archive" /></span>
	<div>
		<h1>Backups</h1>
		<p>Every host sends its backups to the other two over the mesh. Each host here shows what it has received, the backup jobs it runs, and the health of its disks. A root-only scan reads them every {Math.round((data?.scan_every_s ?? 900) / 60)} minutes, with no network and without opening any files.</p>
	</div>
</div>

{#if error}<p class="foot-note" style="color:var(--critical)">{error}</p>{/if}

{#if data && !data.configured}
	<Panel title="Not set up"><p class="sub">Point <span class="mono">backups</span> in obs-beacond.yaml at a backups file. See docs/formats.md.</p></Panel>
{:else if data}
	<p class="summary"><Status s={bad ? 'serious' : 'good'} word={bad ? `${bad} need a look` : `All ${total} backups current`} /></p>
	{#each data.hosts ?? [] as h (h.host)}
		<div class="mt">
			<Panel title="On {h.host}" aside={h.silent ? 'silent · last scan shown' : h.scan ? `scanned ${ago(h.scan.at)}` : 'no scan yet'} flush>
				{#if rows(h).length === 0}
					<p class="sub none">No backups here; the scan only checks its disks.</p>
				{:else}
				<div class="tbl-wrap flat">
					<table class="tbl">
						<thead><tr><th>Backup</th><th>From</th><th>Newest copy</th><th class="r">Size</th><th class="r">Change</th><th class="r">Kept</th><th>State</th></tr></thead>
						<tbody>
							{#each rows(h) as r (r.key)}
								{@const [s, w] = rowState(h, r.key, r.d)}
								{@const f = finding(h, r.key)}
								<tr>
									<td><b>{r.name}</b></td>
									<td class="sub">{r.from}</td>
									<td>{#if r.d?.count && r.d.modified}<span title="{r.d.path} · {hmz(r.d.modified)}">{ago(r.d.modified)}</span> <span class="sub mono">{base(r.d.path)}</span>{:else}<span class="sub">—</span>{/if}</td>
									<td class="r mono">{r.d?.count ? bytes(r.d.size) : '—'}</td>
									<td class="r mono">{change(r.d)}</td>
									<td class="r mono">{r.d?.count ?? '—'}</td>
									<td><Status {s} word={w} />{#if f}<div class="sub det">{f.title}</div>{/if}</td>
								</tr>
							{/each}
						</tbody>
					</table>
				</div>
				{/if}
			</Panel>
			{#if h.scan}
				<div class="grid g2 sub2">
					<Panel title="Backup jobs" aside="systemd">
						<ul class="list">
							{#each h.scan.jobs as j (j.unit)}
								<li><span class="mono">{j.unit}</span><span class="jr"><Status s={jobSev(j)} word={j.result || 'missing'} />{#if j.finished}<span class="sub" title={hmz(j.finished)}>{ago(j.finished)}</span>{/if}</span></li>
							{:else}
								<li class="sub">No jobs on this host.</li>
							{/each}
						</ul>
					</Panel>
					<Panel title="Disks" aside="RAID · SMART · backup space">
						<ul class="list">
							{#each h.scan.raid as r (r.dev)}<li><span class="mono">{r.dev}</span><Status s={r.state.includes('_') ? 'critical' : 'good'} word={r.state.includes('_') ? `degraded ${r.state}` : `mirrored ${r.state}`} /></li>{/each}
							{#each h.scan.smart as d (d.disk)}<li><span class="mono">/dev/{d.disk}</span><Status s={d.error ? 'warning' : d.ok ? 'good' : 'critical'} word={d.error ? "can't read" : d.ok ? 'SMART passed' : 'SMART failing'} /></li>{/each}
							{#each h.scan.roots as r (r.path)}<li><span class="mono">{r.path}</span><Status s={r.used_pct >= (data.disk_warn ?? 0.9) * 100 ? 'warning' : 'good'} word="{Math.round(r.used_pct)}% used" /></li>{/each}
						</ul>
						{#each h.scan.errors ?? [] as e (e)}<p class="sub det">{e}</p>{/each}
					</Panel>
				</div>
			{/if}
		</div>
	{/each}
	<p class="foot-note">
		A copy older than {data.max_age_h} hours, missing or empty is serious: email and Discord, not the phone. A copy under {Math.round((data.shrink ?? 0.5) * 100)}% of the one before warns, as does a backup disk over {Math.round((data.disk_warn ?? 0.9) * 100)}% full. A failed job is serious. A degraded RAID mirror or a failing disk is critical; on a workstation, everything stops at a warning.
		Expectations live in backups.yaml (hosts-baseline).
	</p>
{:else}
	<p class="empty">Loading…</p>
{/if}

<style>
	.flat { border: 0; border-radius: 0; }
	.summary { margin: 0; }
	.sub2 { margin-top: 12px; }
	.list { list-style: none; margin: 0; padding: 0; display: grid; gap: 8px; }
	.list li { display: flex; justify-content: space-between; gap: 12px; align-items: center; flex-wrap: wrap; }
	.det { overflow-wrap: anywhere; max-width: 320px; }
	.none { margin: 0; padding: 12px 16px; }
	.jr { display: inline-flex; align-items: center; gap: 8px; }
</style>
