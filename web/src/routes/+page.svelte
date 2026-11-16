<script lang="ts">
	// The dashboard as a briefing (#69): first a sentence on whether
	// anything needs you, then what does, what can wait, the services, what
	// happened since you last looked and the fleet in a line. Readings live
	// on the Hosts page; here only what someone has to act on or know.
	import Icon from '$lib/Icon.svelte';
	import Status from '$lib/Status.svelte';
	import Strip from '$lib/Strip.svelte';
	import { live } from '$lib/live.svelte';
	import { post, type Attention, type TodayEvent } from '$lib/api';
	import { ago, duration, hm, hour } from '$lib/format';

	let now = $state(Date.now());
	$effect(() => {
		const t = setInterval(() => (now = Date.now()), 1000);
		return () => clearInterval(t);
	});

	const o = $derived(live.overview);
	const total = $derived(o?.fleet?.hosts ?? 0);
	const reporting = $derived(o?.fleet?.reporting ?? 0);
	const attn = $derived(o?.attention ?? []);
	const urgent = (a: Attention) => a.severity === 'critical' || a.severity === 'serious';
	// Picked up by someone: out of "Needs you", shown as being handled.
	const taken = $derived(attn.filter((a) => a.incident?.ack));
	const needs = $derived(attn.filter((a) => urgent(a) && !a.incident?.ack));
	const later = $derived(attn.filter((a) => !urgent(a) && !a.incident?.ack));
	const services = $derived(o?.services ?? []);
	const working = $derived(services.filter((s) => s.status === 'good' || s.status === 'off').length);
	const customer = $derived(services.filter((s) => s.tier === 'public'));
	const custDown = $derived(customer.filter((s) => s.status === 'critical'));
	const custHurt = $derived(customer.filter((s) => s.status === 'warning' || s.status === 'serious'));
	const clusters = $derived(o?.clusters ?? []);
	const clusterTrouble = $derived(clusters.filter((k) => k.status !== 'good' && k.status !== 'off').length);
	const c = $derived(o?.containers);
	const today = $derived(o?.today);
	const watch = $derived(o?.watch);

	const NUM = ['No', 'One', 'Two', 'Three', 'Four', 'Five', 'Six', 'Seven', 'Eight', 'Nine', 'Ten'];
	const num = (n: number) => (n <= 10 ? NUM[n] : String(n));
	const names = (l: { name: string }[]) => (l.length <= 2 ? l.map((s) => s.name).join(' and ') : `${l[0].name} and ${l.length - 1} more`);

	const headline = $derived(
		needs.length ? `${num(needs.length)} ${needs.length === 1 ? 'thing needs' : 'things need'} you.` : later.length ? 'Nothing needs you right now.' : 'All quiet. Nothing needs you.'
	);
	const customers = $derived(
		!customer.length ? '' : custDown.length ? `${names(custDown)} ${custDown.length === 1 ? 'is' : 'are'} down for customers.` : custHurt.length ? `Customers may notice trouble with ${names(custHurt)}.` : 'Customers aren’t affected.'
	);
	const subline = $derived.by(() => {
		const parts: string[] = [];
		if (total) parts.push(reporting === total ? `All ${total} hosts are reporting` : `${reporting} of ${total} hosts are reporting, ${total - reporting} gone quiet`);
		if (services.length) parts.push(working === services.length ? 'every service is working' : `${working} of ${services.length} services are fully working`);
		let s = parts.join(', and ');
		if (s) s = s[0].toUpperCase() + s.slice(1) + '.';
		if (!needs.length && later.length) s += ` ${num(later.length)} ${later.length === 1 ? 'thing' : 'things'} can wait.`;
		return s.trim();
	});

	const hello = $derived(hour(now) < 12 ? 'Good morning' : hour(now) < 18 ? 'Good afternoon' : 'Good evening');
	const firstName = $derived((live.session?.user ?? '').split(' ')[0]);
	const dayFmt = new Intl.DateTimeFormat(undefined, { weekday: 'long', month: 'long', day: 'numeric' });
	const checked = $derived(live.updated ? Math.max(0, Math.round((now - live.updated) / 1000)) : -1);

	// "I'm on it" is the acknowledgement: the repeats stop and everyone sees who has it.
	const canAck = $derived(live.seesAll && !live.session?.demo);
	let busy = $state('');
	let error = $state('');
	async function onIt(a: Attention) {
		if (!a.incident) return;
		busy = a.incident.id;
		error = '';
		try {
			await post(`/api/v1/incidents/${encodeURIComponent(a.incident.id)}/ack`);
			await live.refresh();
		} catch (e) {
			error = String(e);
		} finally {
			busy = '';
		}
	}

	const outcome = (e: TodayEvent): [string, string] =>
		!e.resolved ? (e.ack_by ? [`${e.ack_by} is on it`, 'warning'] : ['Still open', e.severity === 'critical' ? 'critical' : 'warning']) : e.ack_by ? ['Fixed', 'good'] : ['Fixed itself', 'good'];
	const upPct = (p: number) => (p < 0 ? '–' : p >= 99.995 ? '100%' : p.toFixed(2) + '%');
	const days = (d: number) => (d < 1 ? 'within a day' : `in about ${Math.round(d)} ${Math.round(d) === 1 ? 'day' : 'days'}`);
</script>

<svelte:head><title>Cairn OBS · Today</title></svelte:head>

<div class="today">
	<header class="lead">
		<div class="words">
			<p class="hi">{hello}{firstName ? ', ' + firstName : ''} · {dayFmt.format(now)}</p>
			{#if o}
				<h1>{headline} {#if needs.length}<span class="cust">{customers}</span>{/if}</h1>
				{#if subline || (!needs.length && customers)}<p class="subline">{!needs.length && customers ? customers + ' ' : ''}{subline}</p>{/if}
			{:else}
				<h1>Looking…</h1>
			{/if}
		</div>
		<p class="live" class:stale={!!live.error}>
			<i></i>{live.error ? live.error : checked < 0 ? 'Connecting' : checked < 3 ? 'Live · checked just now' : `Live · checked ${checked} s ago`}
		</p>
	</header>

	{#if o}
		{#if error}<p class="err" role="alert">{error}</p>{/if}

		{#if needs.length || taken.length}
			<section aria-labelledby="needs">
				<div class="sh">
					<h2 id="needs">Needs you{#if needs.length}<span> · {needs.length}</span>{/if}</h2>
					<p>Worst first. {needs.length ? 'Nobody has picked these up yet.' : 'Everything urgent has someone on it.'}</p>
				</div>
				{#each needs as a (a.key || a.title)}
					<article class="card need {a.severity}">
						<div class="meta">
							<Status s={a.severity} />
							<span>
								{a.host ? a.host + ' · ' : ''}{a.incident ? 'started ' + ago(a.incident.opened, now) : 'just now'}{a.incident?.fired ? ' · people were told' : ''}
							</span>
						</div>
						<h3>{a.title}</h3>
						{#if a.detail}<p class="why">{a.detail}</p>{/if}
						{#if a.affected}<p class="touch"><b>What it touches</b> {a.affected}</p>{/if}
						<div class="acts">
							{#if a.incident && canAck}
								<button class="btn pri big" disabled={busy === a.incident.id} onclick={() => onIt(a)}>{busy === a.incident.id ? 'Saving…' : 'I’m on it'}</button>
							{:else if a.incident && live.session?.demo}
								<button class="btn pri big" disabled title="The demo changes nothing">I’m on it</button>
							{/if}
							<a class="btn big" href={a.href}>Open</a>
							{#if a.incident}<a class="btn big ghost" href="/alerts?q={encodeURIComponent(a.incident.id)}">{a.incident.id}</a>{/if}
						</div>
					</article>
				{/each}
				{#if taken.length}
					<div class="taken">
						<h3>Being handled · {taken.length}</h3>
						<ul>
							{#each taken as a (a.key || a.title)}
								<li>
									<span class="sev {a.severity}" title={a.severity}><Icon name={a.severity} size={16} /></span>
									<a href={a.href}>{a.title}</a>
									<span class="who">{a.incident?.ack?.by} is on it · {ago(a.incident?.ack?.at ?? now, now)}</span>
								</li>
							{/each}
						</ul>
					</div>
				{/if}
			</section>
		{/if}

		{#if later.length}
			<section aria-labelledby="later">
				<div class="sh">
					<h2 id="later">Can wait<span> · {later.length}</span></h2>
					<p>Worth a look today. None of these is hurting anything yet.</p>
				</div>
				<ul class="card rows">
					{#each later.slice(0, 6) as a (a.key || a.title)}
						<li>
							<span class="ic"><span class="sev {a.severity}" title={a.severity}><Icon name={a.severity} size={16} /></span></span>
							<span class="txt"><b>{a.title}</b>{#if a.detail}<small>{a.detail}</small>{/if}</span>
							{#if a.incident}<span class="age">{duration((now - new Date(a.incident.opened).getTime()) / 1000)}</span>{/if}
							<a class="open" href={a.href}>Open</a>
						</li>
					{/each}
				</ul>
				{#if later.length > 6}<a class="more" href="/alerts">And {later.length - 6} more on the Alerts page</a>{/if}
			</section>
		{/if}

		{#if !needs.length && !later.length && !taken.length}
			<section class="card calm" aria-label="Nothing needs you">
				<span class="big-ok"><Icon name="good" size={36} /></span>
				<div>
					<h2>Nothing needs you right now</h2>
					<p>Cairn OBS tells the alert channels if that changes.{watch?.filling.length ? ` Next to look at: ${watch.filling[0].host}'s ${watch.filling[0].mount}, which fills ${days(watch.filling[0].full_days)}.` : ''}</p>
				</div>
				<a href="/reports">This week’s report</a>
			</section>
		{/if}

		{#if services.length}
			<section aria-labelledby="svc">
				<div class="sh">
					<h2 id="svc">Your services</h2>
					<p>Last 24 hours, one bar per half hour · <a href="/map/services">Service map</a></p>
				</div>
				<div class="tiles">
					{#each services as s (s.id)}
						<a class="card tile" href="/services?s={encodeURIComponent(s.id)}">
							<b>{s.name}</b>
							<Status s={s.status} word={s.status === 'good' ? 'Working' : s.word} />
							<Strip cells={s.history.cells} name={s.name} />
							<small>{s.history.pct_24h < 0 ? 'No history yet' : `Up ${upPct(s.history.pct_24h)} today · ${upPct(s.history.pct_30d)} in 30 days`}</small>
						</a>
					{/each}
				</div>
			</section>
		{/if}

		{#if today}
			<section aria-labelledby="since">
				<div class="sh">
					<h2 id="since">Since you last looked</h2>
					<p>{today.last_visit ? `You were last here ${ago(today.since, now)}, at ${hm(today.since)}` : 'The last 12 hours'}</p>
				</div>
				{#if today.events.length}
					<ol class="card rows events">
						{#each today.events as e (e.id)}
							{@const [word, tone] = outcome(e)}
							<li>
								<time class="mono" datetime={e.at}>{hm(e.at)}</time>
								<span class="ic sev {e.severity}" title={e.severity}><Icon name={e.severity} size={16} /></span>
								<span class="txt">{#if e.href}<a href={e.href}>{e.title}</a>{:else}{e.title}{/if}</span>
								<span class="out {tone}">{word}</span>
							</li>
						{/each}
					</ol>
					<p class="foot">
						{today.opened} opened, {today.resolved} closed{today.fixed_itself ? ` (${today.fixed_itself} on their own)` : ''}. {today.told ? `${today.told} ${today.told === 1 ? 'alert' : 'alerts'} went out to people.` : 'Nobody was alerted.'}
						{#if today.opened + today.resolved > today.events.length}<a href="/alerts">Everything on the Alerts page</a>{/if}
					</p>
				{:else}
					<p class="card quiet">Nothing opened or closed since then.</p>
				{/if}
			</section>
		{/if}

		<section aria-labelledby="fleet">
			<div class="sh">
				<h2 id="fleet">The fleet</h2>
				<p><a href="/hosts">All hosts</a>{#if clusters.length} · <a href="/clusters">Clusters</a>{/if}</p>
			</div>
			<div class="facts">
				<a class="card fact" href="/hosts"><b>{reporting} of {total}</b><span>hosts reporting</span></a>
				{#if c}<a class="card fact" href="/containers"><b>{c.running + c.problems}</b><span>{c.problems ? `containers, ${c.problems} need a look` : 'containers, all where they should be'}</span></a>{/if}
				{#if clusters.length}<a class="card fact" href="/clusters"><b>{clusters.length} {clusters.length === 1 ? 'cluster' : 'clusters'}</b><span>{clusterTrouble ? `${clusterTrouble} with a problem` : 'all healthy'}</span></a>{/if}
				{#if o.fleet?.busiest}<a class="card fact" href="/hosts/{o.fleet.busiest.host}"><b>{Math.round(o.fleet.busiest.cpu_pct)}% CPU</b><span>busiest host, {o.fleet.busiest.host}</span></a>{/if}
			</div>
			{#if watch && (watch.filling.length || watch.busier.length || watch.restarted_n)}
				<div class="card watch">
					<h3>Worth watching</h3>
					<ul>
						{#each watch.filling as d (d.host + d.mount)}
							<li><a class="mono" href="/hosts/{d.host}">{d.host}</a> <span>{d.mount} is {Math.round(d.pct)}% full and fills {days(d.full_days)}</span></li>
						{/each}
						{#each watch.busier as b (b.host + b.what)}
							<li><a class="mono" href="/hosts/{b.host}">{b.host}</a> <span>{b.what === 'cpu' ? 'CPU' : 'memory'} at {Math.round(b.now)}%, usually {Math.round(b.usual)}% at this hour</span></li>
						{/each}
						{#if watch.restarted_n}
							<li>
								<span>{watch.restarted_n === 1 ? 'Restarted' : `${watch.restarted_n} hosts restarted`} in the last day:</span>
								{#each watch.restarted as r, i (r.host)}{i ? ', ' : ' '}<a class="mono" href="/hosts/{r.host}">{r.host}</a> <span class="sub">{duration(r.up_s)} ago</span>{/each}{watch.restarted_n > watch.restarted.length ? ` and ${watch.restarted_n - watch.restarted.length} more` : ''}
							</li>
						{/if}
					</ul>
				</div>
			{/if}
		</section>
	{/if}
</div>

<style>
	.today { max-width: 1120px; margin: 0 auto; display: flex; flex-direction: column; gap: 40px; font-size: 15px; padding-bottom: 48px; }
	.lead { display: flex; flex-wrap: wrap; gap: 16px 32px; justify-content: space-between; align-items: flex-start; padding-top: 8px; }
	.words { display: flex; flex-direction: column; gap: 10px; max-width: 800px; min-width: 0; }
	.hi { margin: 0; color: var(--muted-fg); }
	.lead h1 { font-size: clamp(26px, 4vw, 38px); line-height: 1.18; letter-spacing: -.01em; }
	.lead h1 .cust { color: var(--muted-fg); font-weight: 500; }
	.subline { margin: 0; font-size: 17px; color: color-mix(in oklab, var(--foreground) 82%, var(--muted-fg)); }
	.live { margin: 0; display: inline-flex; align-items: center; gap: 8px; padding: 6px 14px; border: 1px solid var(--border); border-radius: 999px; color: var(--muted-fg); font-size: 13px; }
	.live i { width: 8px; height: 8px; border-radius: 50%; background: var(--good); }
	.live.stale i { background: var(--critical); }
	.err { margin: 0; color: var(--critical); }
	.sev { display: inline-flex; color: var(--muted-fg); }
	.sev.good { color: var(--good); } .sev.warning { color: var(--warning); } .sev.serious { color: var(--serious); } .sev.critical { color: var(--critical); }

	section { display: flex; flex-direction: column; gap: 14px; min-width: 0; }
	.sh { display: flex; align-items: baseline; justify-content: space-between; gap: 6px 16px; flex-wrap: wrap; }
	.sh h2 { font-size: 21px; }
	.sh h2 span { color: var(--muted-fg); font-weight: 400; }
	.sh p { margin: 0; color: var(--muted-fg); font-size: 14px; }
	.card { background: var(--card); border: 1px solid var(--border); border-radius: 12px; box-shadow: var(--shadow-soft); }

	.need { padding: 22px 24px; display: flex; flex-direction: column; gap: 12px; }
	.need.critical { border-color: color-mix(in oklab, var(--critical) 45%, var(--border)); }
	.need.serious { border-color: color-mix(in oklab, var(--serious) 45%, var(--border)); }
	.meta { display: flex; flex-wrap: wrap; gap: 6px 14px; align-items: center; color: var(--muted-fg); font-size: 14px; }
	.need h3 { font-size: 22px; line-height: 1.3; }
	.why { margin: 0; max-width: 75ch; color: color-mix(in oklab, var(--foreground) 85%, var(--muted-fg)); font-size: 16px; }
	.touch { margin: 0; font-size: 15px; }
	.touch b { color: var(--muted-fg); font-weight: 500; margin-right: 6px; }
	.acts { display: flex; flex-wrap: wrap; gap: 10px; margin-top: 4px; }
	.btn.big { height: 44px; padding: 0 20px; font-size: 15px; border-radius: 8px; }
	.btn.pri.big { font-weight: 700; }
	.btn.ghost { background: transparent; color: var(--muted-fg); }

	.taken { padding: 4px 4px 0; }
	.taken h3 { font-size: 15px; color: var(--muted-fg); font-weight: 600; margin-bottom: 6px; }
	.taken ul { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 6px; }
	.taken li { display: flex; flex-wrap: wrap; align-items: center; gap: 4px 10px; }
	.taken a { color: var(--foreground); }
	.who { color: var(--muted-fg); font-size: 14px; }

	.rows { list-style: none; margin: 0; padding: 0; overflow: hidden; }
	.rows li { display: flex; gap: 14px; align-items: baseline; padding: 14px 20px; border-top: 1px solid var(--border); min-width: 0; }
	.rows li:first-child { border-top: 0; }
	.rows .ic { flex: none; align-self: flex-start; padding-top: 1px; }
	.rows .txt { flex: 1 1 auto; min-width: 0; display: flex; flex-direction: column; gap: 2px; }
	.rows .txt b { font-weight: 600; }
	.rows .txt small { color: var(--muted-fg); font-size: 14px; }
	.rows .age { flex: none; color: var(--muted-fg); font-size: 14px; white-space: nowrap; }
	.rows .open { flex: none; font-weight: 600; font-size: 14px; }
	.more { align-self: flex-start; }

	.calm { padding: 26px; display: flex; flex-wrap: wrap; gap: 18px; align-items: center; }
	.calm div { flex: 1 1 320px; }
	.calm h2 { font-size: 21px; }
	.calm p { margin: 6px 0 0; color: color-mix(in oklab, var(--foreground) 85%, var(--muted-fg)); }
	.big-ok { color: var(--good); display: inline-flex; }

	.tiles { display: grid; grid-template-columns: repeat(auto-fill, minmax(200px, 1fr)); gap: 12px; }
	.tile { display: flex; flex-direction: column; gap: 10px; padding: 16px; color: var(--foreground); min-width: 0; }
	.tile:hover { border-color: color-mix(in oklab, var(--primary) 50%, var(--border)); }
	.tile b { font-size: 16px; }
	.tile small { color: var(--muted-fg); font-size: 13px; }

	.events time { flex: 0 0 52px; color: var(--data); font-size: 14px; }
	.events .txt a { color: var(--foreground); }
	.events .out { flex: none; font-size: 13px; font-weight: 600; white-space: nowrap; }
	.out.good { color: var(--good); } .out.warning { color: var(--warning); } .out.critical { color: var(--critical); }
	.foot { margin: 0; color: var(--muted-fg); font-size: 14px; display: flex; flex-wrap: wrap; gap: 4px 12px; }
	.quiet { margin: 0; padding: 16px 20px; color: var(--muted-fg); }

	.facts { display: grid; grid-template-columns: repeat(auto-fit, minmax(210px, 1fr)); gap: 12px; }
	.fact { padding: 16px 20px; color: var(--foreground); display: flex; flex-direction: column; gap: 2px; }
	.fact:hover { border-color: color-mix(in oklab, var(--primary) 50%, var(--border)); }
	.fact b { font: 600 26px var(--sans); color: var(--data); }
	.fact span { color: color-mix(in oklab, var(--foreground) 85%, var(--muted-fg)); }
	.watch { padding: 18px 20px; }
	.watch h3 { font-size: 16px; margin-bottom: 8px; }
	.watch ul { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 8px; }
	.watch li span { color: color-mix(in oklab, var(--foreground) 85%, var(--muted-fg)); }

	@media (max-width: 640px) {
		.today { gap: 32px; }
		.need { padding: 18px; }
		.acts .btn.pri.big { flex: 1 1 100%; justify-content: center; }
		.rows li { padding: 12px 14px; flex-wrap: wrap; row-gap: 4px; }
		.rows .txt { flex-basis: calc(100% - 40px); }
		.rows .age { margin-left: 30px; }
		.events time { flex-basis: 44px; }
	}
</style>
