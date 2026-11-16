<script lang="ts">
	// One square per host, grouped by site and role (#72): at thousands of
	// hosts a table hides the one that's wrong; a field of calm squares
	// doesn't. Above it, the few hosts heading somewhere.
	import { get, SignedOut, type Watch } from './api';
	import { live } from './live.svelte';
	import { duration } from './format';

	interface GridHost { name: string; status: string; word: string }
	interface GridSite { id: string; name: string; hosts: number; roles: { role: string; hosts: GridHost[] }[]; trouble: GridHost[] }

	let sites = $state<GridSite[] | null>(null);
	let watch = $state<Watch | null>(null);
	let error = $state('');
	async function load() {
		try {
			const r = await get<{ sites: GridSite[]; watch: Watch }>('/api/v1/fleet');
			sites = r.sites;
			watch = r.watch;
			error = '';
		} catch (e) {
			if (e instanceof SignedOut) void live.checkSession();
			else error = String(e);
		}
	}
	$effect(() => {
		void load();
		const t = setInterval(() => void load(), 10_000);
		return () => clearInterval(t);
	});

	const total = $derived((sites ?? []).reduce((n, s) => n + s.hosts, 0));
	// Smaller squares for a bigger fleet, so a site still fits on a screen.
	const size = $derived(total > 2000 ? 8 : total > 600 ? 11 : 18);
	const WORD: Record<string, string> = { good: 'fine', warning: 'warning', serious: 'serious', critical: 'critical', waiting: 'waiting for its first report', off: 'off' };
	const days = (d: number) => (d < 1 ? 'within a day' : `in about ${Math.round(d)} ${Math.round(d) === 1 ? 'day' : 'days'}`);
	const ROLE: Record<string, string> = { app: 'App', db: 'Database', lb: 'Load balancer', gw: 'Gateway', mx: 'Mail', mq: 'Messaging', cache: 'Cache', web: 'Web', stor: 'Storage', win: 'Windows', ibmi: 'IBM i', virt: 'Virtualization', kvm: 'KVM', k8s: 'Kubernetes', host: 'Host', other: 'Other' };
	const roleName = (r: string) => ROLE[r] ?? r.charAt(0).toUpperCase() + r.slice(1);
</script>

{#if error}<p class="foot-note" style="color:var(--critical)">{error}</p>{/if}

{#if watch && (watch.filling.length || watch.busier.length || watch.restarted_n)}
	<section class="watch" aria-labelledby="watch-h">
		<h2 id="watch-h">Worth watching</h2>
		<div class="wcols">
			{#if watch.filling.length}
				<div class="card">
					<h3>Filling up</h3>
					<ul>{#each watch.filling as d (d.host + d.mount)}<li><a class="mono" href="/hosts/{d.host}">{d.host}</a><span class="val mono">{Math.round(d.pct)}%</span><span class="bar"><i style="width:{Math.min(100, d.pct)}%"></i></span><small>{d.mount} · full {days(d.full_days)}</small></li>{/each}</ul>
				</div>
			{/if}
			{#if watch.busier.length}
				<div class="card">
					<h3>Busier than usual</h3>
					<ul>{#each watch.busier as b (b.host + b.what)}<li><a class="mono" href="/hosts/{b.host}">{b.host}</a><span class="val mono">{Math.round(b.now)}% {b.what === 'cpu' ? 'CPU' : 'memory'}</span><span class="bar"><i style="width:{Math.min(100, b.now)}%"></i></span><small>usually {Math.round(b.usual)}% at this hour</small></li>{/each}</ul>
				</div>
			{/if}
			{#if watch.restarted_n}
				<div class="card">
					<h3>Restarted in the last day · {watch.restarted_n}</h3>
					<ul>{#each watch.restarted as r (r.host)}<li><a class="mono" href="/hosts/{r.host}">{r.host}</a><small>{duration(r.up_s)} ago</small></li>{/each}</ul>
					{#if watch.restarted_n > watch.restarted.length}<p class="sub">and {watch.restarted_n - watch.restarted.length} more</p>{/if}
				</div>
			{/if}
		</div>
	</section>
{/if}

{#if sites === null}
	<p class="empty">Loading…</p>
{:else}
	<div class="legend" aria-hidden="true">
		<span><i class="sq good"></i>Fine</span>
		<span><i class="sq warning"></i>Warning</span>
		<span><i class="sq serious"></i>Serious</span>
		<span><i class="sq critical"></i>Critical or silent</span>
		<span><i class="sq waiting"></i>Waiting</span>
	</div>
	<div class="sites" style="--sq:{size}px">
		{#each sites as s (s.id)}
			<section class="card site" aria-labelledby="site-{s.id || 'none'}">
				<div class="sh">
					<h2 id="site-{s.id || 'none'}">{s.name}</h2>
					<span class:bad={s.trouble.length}>{s.hosts} {s.hosts === 1 ? 'host' : 'hosts'} · {s.trouble.length ? `${s.trouble.length} need a look` : 'all fine'}</span>
				</div>
				{#each s.roles as r (r.role)}
					<div class="role">
						<span class="rl">{roleName(r.role)} · {r.hosts.length}</span>
						<span class="sqs">
							{#each r.hosts as h (h.name)}<a class="sq {h.status}" href="/hosts/{h.name}" title="{h.name}: {h.status === 'good' ? 'fine' : h.word || WORD[h.status]}" aria-label="{h.name}, {h.status === 'good' ? 'fine' : h.word || WORD[h.status]}"></a>{/each}
						</span>
					</div>
				{/each}
				{#if s.trouble.length}
					<ul class="trouble">
						{#each s.trouble.slice(0, 8) as h (h.name)}<li><i class="dot {h.status}"></i><a class="mono" href="/hosts/{h.name}">{h.name}</a> <span>{h.word}</span></li>{/each}
						{#if s.trouble.length > 8}<li class="sub">and {s.trouble.length - 8} more</li>{/if}
					</ul>
				{/if}
			</section>
		{/each}
	</div>
{/if}

<style>
	.card { background: var(--card); border: 1px solid var(--border); border-radius: 12px; box-shadow: var(--shadow-soft); }
	.watch { margin-bottom: 20px; }
	.watch h2 { font-size: 18px; margin-bottom: 10px; }
	.wcols { display: grid; grid-template-columns: repeat(auto-fit, minmax(280px, 1fr)); gap: 12px; align-items: start; }
	.wcols .card { padding: 16px 18px; }
	.wcols h3 { font-size: 15px; margin-bottom: 10px; }
	.wcols ul { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 10px; }
	.wcols li { display: grid; grid-template-columns: 1fr auto; gap: 2px 10px; }
	.wcols .val { color: var(--data); font-size: 13px; }
	.wcols .bar { grid-column: 1 / -1; height: 5px; border-radius: 3px; background: var(--border); display: flex; }
	.wcols .bar i { border-radius: 3px; background: var(--data); }
	.wcols small { grid-column: 1 / -1; color: var(--muted-fg); font-size: 13px; }

	.legend { display: flex; flex-wrap: wrap; gap: 8px 18px; margin: 4px 0 14px; color: var(--muted-fg); font-size: 13px; }
	.legend span { display: inline-flex; align-items: center; gap: 6px; }
	.legend .sq { width: 14px; height: 14px; }
	.sites { display: grid; grid-template-columns: repeat(auto-fit, minmax(340px, 1fr)); gap: 16px; align-items: start; }
	.site { padding: 18px 20px; display: flex; flex-direction: column; gap: 10px; min-width: 0; }
	.sh { display: flex; justify-content: space-between; align-items: baseline; gap: 10px; flex-wrap: wrap; }
	.sh h2 { font-size: 19px; }
	.sh span { color: var(--muted-fg); font-size: 14px; }
	.sh span.bad { color: var(--warning); }
	.role { display: flex; gap: 12px; align-items: flex-start; }
	.rl { flex: 0 0 110px; color: var(--muted-fg); font-size: 13px; line-height: var(--sq); }
	.sqs { display: flex; flex-wrap: wrap; gap: 3px; flex: 1 1 auto; min-width: 0; }
	.sq { display: block; width: var(--sq); height: var(--sq); border-radius: 3px; background: color-mix(in oklab, var(--good) 38%, var(--card)); }
	a.sq:hover, a.sq:focus-visible { outline: 2px solid var(--primary); outline-offset: 1px; }
	.sq.warning { background: var(--warning); }
	.sq.serious { background: var(--serious); }
	.sq.critical { background: var(--critical); }
	.sq.waiting, .sq.off { background: var(--input); }
	.trouble { list-style: none; margin: 4px 0 0; padding: 10px 0 0; border-top: 1px solid var(--border); display: flex; flex-direction: column; gap: 6px; font-size: 14px; }
	.trouble li { display: flex; align-items: baseline; gap: 8px; min-width: 0; }
	.trouble li span { color: color-mix(in oklab, var(--foreground) 85%, var(--muted-fg)); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; min-width: 0; }
	.dot { flex: none; width: 10px; height: 10px; border-radius: 2px; background: var(--warning); align-self: center; }
	.dot.critical { background: var(--critical); } .dot.serious { background: var(--serious); }
	@media (max-width: 640px) { .rl { flex-basis: 84px; } }
</style>
