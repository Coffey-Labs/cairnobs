<script lang="ts">
	import Icon from '$lib/Icon.svelte';
	import Panel from '$lib/Panel.svelte';
	import Spark from '$lib/Spark.svelte';
	import Status from '$lib/Status.svelte';
	import { get, SignedOut, type CheckView, type Severity } from '$lib/api';
	import { live } from '$lib/live.svelte';
	import { ago, duration, hmz } from '$lib/format';

	type SignIn = { name: string; ok: boolean; ms: number; error?: string; fails: number };
	type Snapshot = {
		checked: string;
		error?: string;
		signins: SignIn[] | null;
		trip: { at: string; ok: boolean; seconds: number; error?: string; fails: number };
		history: { at: string; seconds: number }[] | null;
		queue: { read: boolean; total: number; oldest?: string; error?: string; fails: number };
	};
	type Cond = { key: string; severity: Severity; title: string; detail: string };
	type View = {
		configured: boolean; account?: string; server?: string; every_s?: number; wait_s?: number;
		round_trip_warn_s?: number; queue_warn?: number; queue_critical?: number;
		snapshot?: Snapshot; conditions?: Cond[]; checks?: CheckView[];
	};

	let data = $state<View | null>(null);
	let error = $state('');
	async function load() {
		try {
			data = await get<View>('/api/v1/mail');
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

	const s = $derived(data?.snapshot);
	const started = $derived(!!s && !s.checked.startsWith('0001'));
	const hist = $derived(s?.history ?? []);
	const arrived = $derived(hist.filter((p) => p.seconds >= 0));
	const missed = $derived(hist.length - arrived.length);
	const median = $derived.by(() => {
		const v = arrived.map((p) => p.seconds).sort((a, b) => a - b);
		return v.length ? v[Math.floor(v.length / 2)] : 0;
	});
	const secs = (n: number) => (n < 10 ? n.toFixed(1) : n < 90 ? Math.round(n).toString() : '');
	function tripState(): [Severity, string] {
		if (!s) return ['waiting', 'Waiting'];
		const t = s.trip;
		if (!t.ok) return [t.fails >= 2 ? 'critical' : 'warning', 'Not arrived'];
		if (t.seconds > (data?.round_trip_warn_s ?? 60)) return ['warning', 'Slow'];
		return ['good', 'Arrived'];
	}
	function queueState(): [Severity, string] {
		const q = s?.queue;
		if (!q?.read) return [q && q.fails >= 2 ? 'warning' : 'waiting', 'Not read'];
		if (q.total >= (data?.queue_critical ?? 500)) return ['critical', 'Backed up'];
		if (q.total >= (data?.queue_warn ?? 50)) return ['warning', 'Growing'];
		return ['good', q.total ? 'Moving' : 'Empty'];
	}
	const signSev = (x: SignIn): Severity => (x.ok ? 'good' : x.fails >= 2 ? 'critical' : 'warning');
	const rbl = $derived((data?.checks ?? []).filter((c) => c.kind === 'rbl'));
	const ports = $derived((data?.checks ?? []).filter((c) => c.kind !== 'rbl'));
	const rblClean = $derived(rbl.filter((c) => c.status === 'good').length);
</script>

<svelte:head><title>Cairn OBS · Mail</title></svelte:head>

<div class="phead">
	<span class="tile"><Icon name="mail" /></span>
	<div>
		<h1>Mail</h1>
		<p>Mail the way a person uses it. Cairn OBS's own account signs in over SMTP, IMAP and JMAP, sends itself a message and times how long it takes to arrive. Every round it also reads the outgoing queue.</p>
	</div>
</div>

{#if error}<p class="foot-note" style="color:var(--critical)">{error}</p>{/if}

{#if data && !data.configured}
	<Panel title="Not set up"><p class="sub">Add a <span class="mono">mail</span> section to obs-beacond.yaml. See docs/formats.md.</p></Panel>
{:else if data && s}
	{#if s.error}<p class="foot-note" style="color:var(--warning)">{s.error}</p>{/if}
	{#if !started}
		<Panel title="First round running"><p class="sub">The first round takes a few seconds. This page refreshes on its own.</p></Panel>
	{:else}
		{@const [ts, tw] = tripState()}
		{@const [qs, qw] = queueState()}
		<div class="grid g4">
			<Panel title="Round trip" aside="every {duration(data.every_s ?? 300)}">
				<div class="stat">
					<div class="val">{#if s.trip.ok}{secs(s.trip.seconds) || duration(Math.round(s.trip.seconds))}<small>{secs(s.trip.seconds) ? ' s' : ''}</small>{:else}—{/if}</div>
					<div class="sub row"><Status s={ts} word={tw} /><span>{ago(s.trip.at)}</span></div>
				</div>
				<Spark data={arrived.map((p) => p.seconds)} label="Round trip in seconds, last day" />
				<div class="sub">{#if arrived.length}typical {secs(median)} s over {arrived.length === 1 ? 'one round' : `${arrived.length} rounds`}{/if}{#if missed} · <span class="miss">{missed} missed</span>{/if}</div>
			</Panel>
			<Panel title="Sign-ins" aside={data.account}>
				<ul class="signins">
					{#each s.signins ?? [] as x (x.name)}
						<li><b>{x.name}</b><Status s={signSev(x)} word={x.ok ? `${Math.round(x.ms)} ms` : 'Refused'} />{#if !x.ok}<div class="sub det">{x.error}</div>{/if}</li>
					{/each}
				</ul>
			</Panel>
			<Panel title="Outgoing queue" aside="whole server">
				<div class="stat">
					<div class="val">{s.queue.read ? s.queue.total : '—'}<small>{s.queue.read ? (s.queue.total === 1 ? ' message' : ' messages') : ''}</small></div>
					<div class="sub row"><Status s={qs} word={qw} />{#if s.queue.oldest}<span>oldest {ago(s.queue.oldest)}</span>{/if}</div>
					{#if s.queue.error}<div class="sub det">{s.queue.error}</div>{/if}
				</div>
			</Panel>
			<Panel title="Block lists" aside="mail IPs" href="/checks" hrefLabel="Checks">
				<div class="stat">
					<div class="val">{rblClean}<small>{` of ${rbl.length} clean`}</small></div>
					{#each rbl.filter((c) => c.status !== 'good') as c (c.id)}<div class="sub det"><Status s={c.status as Severity} word={c.name} /> {c.detail ?? c.word}</div>{/each}
				</div>
			</Panel>
		</div>

		{#if (data.conditions ?? []).length}
			<div class="mt">
				<Panel title="Needs a look">
					<ul class="conds">
						{#each data.conditions ?? [] as c (c.key)}<li><Status s={c.severity} word={c.title} /><div class="sub">{c.detail}</div></li>{/each}
					</ul>
				</Panel>
			</div>
		{/if}

		{#if !s.trip.ok && s.trip.error}<p class="foot-note">Last round: {s.trip.error}.</p>{/if}

		<div class="mt">
			<Panel title="Ports and DNS" aside="from every host's agent" flush>
				<div class="tbl-wrap flat">
					<table class="tbl">
						<thead><tr><th>Check</th><th>Target</th><th>State</th></tr></thead>
						<tbody>
							{#each ports as c (c.id)}
								<tr><td><b>{c.name}</b></td><td class="mono sub">{c.target}</td><td><Status s={c.status as Severity} word={c.word} />{#if c.detail}<div class="sub det">{c.detail}</div>{/if}</td></tr>
							{:else}
								<tr><td colspan="3" class="empty">The service map's mail node lists no checks.</td></tr>
							{/each}
						</tbody>
					</table>
				</div>
			</Panel>
		</div>
		<p class="foot-note">
			Last round {hmz(s.checked)}. A test message gets {duration(data.wait_s ?? 240)} to arrive. One that misses warns, and two rounds in a row is critical. Slower than {data.round_trip_warn_s} s warns.
			The queue warns at {data.queue_warn} messages and is critical at {data.queue_critical}. The monitor's mailbox clears itself.
		</p>
	{/if}
{:else}
	<p class="empty">Loading…</p>
{/if}

<style>
	.flat { border: 0; border-radius: 0; }
	.signins, .conds { list-style: none; margin: 0; padding: 0; display: grid; gap: 10px; }
	.signins li { display: grid; grid-template-columns: 52px 1fr; align-items: center; gap: 2px 10px; }
	.signins .det { grid-column: 2; }
	.det { overflow-wrap: anywhere; }
	.miss { color: var(--warning); }
	.row { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
	.stat + :global(.spark) { margin-top: 10px; }
	:global(.panel) .empty { padding: 20px; }
</style>
