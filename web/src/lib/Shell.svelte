<script lang="ts">
	import type { Snippet } from 'svelte';
	import { page } from '$app/state';
	import Icon from './Icon.svelte';
	import type { IconName } from './icons';
	import Mark from './Mark.svelte';
	import { post, type Features } from './api';
	import { live } from './live.svelte';
	import { hms, zone } from './format';

	let { children }: { children: Snippet } = $props();

	// The sidebar's width and fold are remembered per browser.
	const SIDE_MIN = 180, SIDE_MAX = 420, SIDE_SNAP = 140, SIDE_DEFAULT = 232;
	const stored = (k: string, d: string) => { try { return localStorage.getItem('beacon.' + k) ?? d; } catch { return d; } };
	const store = (k: string, v: string) => { try { localStorage.setItem('beacon.' + k, v); } catch { /* private window */ } };

	let width = $state(Number(stored('sideW', String(SIDE_DEFAULT))) || SIDE_DEFAULT);
	let rail = $state(stored('rail', 'false') === 'true');
	let drawer = $state(false);
	let menu = $state(false);
	let resizing = $state(false);
	let dark = $state(typeof document !== 'undefined' && document.documentElement.dataset.theme !== 'light');

	const path = $derived(page.url.pathname);
	const problems = $derived(live.overview?.containers.problems ?? 0);
	const unacked = $derived(live.overview?.alerts?.unacknowledged ?? 0);
	const checksDown = $derived(live.overview?.checks?.down ?? 0);
	const hostCritical = $derived(live.overview?.fleet?.critical ?? 0);
	const hostLook = $derived(hostCritical + (live.overview?.fleet?.warning ?? 0));
	const containerSeverity = $derived(live.overview?.fleet?.container_severity ?? 'warning');
	const clusters = $derived(live.overview?.clusters ?? []);
	const clusterProblems = $derived(clusters.reduce((n, c) => n + c.problems + (c.status === 'critical' && !c.problems ? 1 : 0), 0));
	const clusterSeverity = $derived(clusters.some((c) => c.status === 'critical') ? 'critical' : 'warning');

	$effect(() => { path; drawer = false; menu = false; });

	// Two sides: watching (only what's set up shows) and Configure, the
	// control plane, for admins. The switch remembers where you were.
	const admin = $derived(live.seesAll); // operators see Configure read-only
	const configuring = $derived(path === '/configure' || path.startsWith('/configure/'));
	// Until the first overview arrives, the sections from last time, so a
	// page loaded straight from its address doesn't flash a short sidebar
	// that then grows (#44). A convenience: without storage it just waits.
	const remembered = (() => {
		try {
			return (JSON.parse(localStorage.getItem('cairnobs.features') ?? 'null') as Features | null) ?? undefined;
		} catch {
			return undefined;
		}
	})();
	const f = $derived(live.overview?.features ?? remembered);
	$effect(() => {
		const now = live.overview?.features;
		if (!now) return;
		try { localStorage.setItem('cairnobs.features', JSON.stringify(now)); } catch { /* private window, storage off */ }
	});
	let lastWatch = $state('/');
	let lastConfigure = $state('/configure');
	$effect(() => {
		if (configuring) lastConfigure = page.url.pathname;
		else lastWatch = page.url.pathname;
	});
	// Section titles fold, remembered per browser. A folded section still
	// shows the page you're on, so you never lose your place.
	const readFolded = (): Record<string, boolean> => { try { return JSON.parse(stored('folded', '{}')) ?? {}; } catch { return {}; } };
	let folded = $state(readFolded());
	function fold(title: string) {
		folded = { ...folded, [title]: !folded[title] };
		store('folded', JSON.stringify(folded));
	}
	type Item = { href: string; icon: IconName; label: string; show?: boolean; exact?: boolean; meta?: number; sev?: string; tip?: string };
	const overview = $derived(([
		{ href: '/', icon: 'dashboard', label: 'Dashboard', exact: true, show: true },
		// One entry for every host: a big fleet's partial list told you little.
		{ href: '/hosts', icon: 'hosts', label: 'Hosts', show: true, meta: hostLook, sev: hostCritical ? 'critical' : 'warning', tip: hostLook ? `${hostLook} need a look` : '' }
	] as Item[]).filter((i) => i.show));
	const infra = $derived(([
		{ href: '/map/network', icon: 'network', label: 'Network map', show: f?.maps },
		{ href: '/map/services', icon: 'services', label: 'Service map', show: f?.maps },
		{ href: '/mail', icon: 'mail', label: 'Mail', show: f?.mail },
		{ href: '/data', icon: 'database', label: 'Data layer', show: f?.data },
		{ href: '/clusters', icon: 'cluster', label: 'Clusters', show: f?.workloads, meta: clusterProblems, sev: clusterSeverity, tip: clusterProblems ? `${clusterProblems} need a look` : '' },
		{ href: '/workloads', icon: 'grid', label: 'Workloads', show: f?.workloads },
		{ href: '/containers', icon: 'box', label: 'Containers', show: f?.containers || problems > 0, meta: problems, sev: containerSeverity, tip: problems ? `${problems} need a look` : '' },
		{ href: '/certificates', icon: 'lock', label: 'Certificates', show: f?.certificates },
		{ href: '/backups', icon: 'archive', label: 'Backups', show: f?.backups },
		{ href: '/cloudflare', icon: 'cloud', label: 'Cloudflare', show: f?.cloudflare },
		{ href: '/akamai', icon: 'globe', label: 'Akamai', show: f?.akamai }
	] as Item[]).filter((i) => i.show));
	const watch = $derived(([
		{ href: '/checks', icon: 'check', label: 'Checks', show: f?.checks || checksDown > 0, meta: checksDown, sev: live.overview?.checks?.severity ?? 'warning', tip: checksDown ? `${checksDown} down` : '' },
		{ href: '/alerts', icon: 'bell', label: 'Alerts', show: true, meta: unacked, sev: 'critical', tip: unacked ? `${unacked} not acknowledged` : '' },
		{ href: '/reports', icon: 'report', label: 'Reports', show: f?.reports }
	] as Item[]).filter((i) => i.show));
	const setup: Item[] = [
		{ href: '/configure', icon: 'settings', label: 'Overview', exact: true },
		{ href: '/configure/hosts', icon: 'hosts', label: 'Hosts' },
		{ href: '/configure/clusters', icon: 'cluster', label: 'Clusters' },
		{ href: '/configure/checks', icon: 'check', label: 'Checks' },
		{ href: '/configure/alerts', icon: 'bell', label: 'Alerts' },
		{ href: '/configure/integrations', icon: 'plug', label: 'Integrations' }
	];
	const access: Item[] = [
		{ href: '/configure/users', icon: 'users', label: 'Users' },
		{ href: '/configure/audit', icon: 'list', label: 'Audit log' }
	];
	const isOn = (i: Item) => (i.exact ? path === i.href : path === i.href || path.startsWith(i.href + '/'));

	function setWidth(w: number, save = false) {
		width = Math.round(Math.min(SIDE_MAX, Math.max(SIDE_MIN, w)));
		if (save) store('sideW', String(width));
	}
	function setRail(on: boolean) {
		rail = on;
		store('rail', String(on));
	}
	function startResize(e: PointerEvent) {
		e.preventDefault();
		const handle = e.currentTarget as HTMLElement;
		handle.setPointerCapture(e.pointerId);
		const left = (handle.parentElement as HTMLElement).getBoundingClientRect().left;
		resizing = true;
		const move = (ev: PointerEvent) => {
			const w = ev.clientX - left;
			if (w < SIDE_SNAP) { if (!rail) { setRail(true); done(); } return; }
			if (rail) { setRail(false); done(); return; }
			setWidth(w);
		};
		const done = () => {
			resizing = false;
			handle.removeEventListener('pointermove', move);
			handle.removeEventListener('pointerup', done);
			handle.removeEventListener('pointercancel', done);
			if (!rail) setWidth(width, true);
		};
		handle.addEventListener('pointermove', move);
		handle.addEventListener('pointerup', done);
		handle.addEventListener('pointercancel', done);
	}
	function resizeKey(e: KeyboardEvent) {
		const step = e.shiftKey ? 48 : 16;
		if (e.key === 'ArrowLeft') { e.preventDefault(); if (!rail && width - step < SIDE_MIN) setRail(true); else if (!rail) setWidth(width - step, true); }
		if (e.key === 'ArrowRight') { e.preventDefault(); if (rail) setRail(false); else setWidth(width + step, true); }
		if (e.key === 'Home') { e.preventDefault(); setRail(true); }
		if (e.key === 'End') { e.preventDefault(); setRail(false); setWidth(SIDE_MAX, true); }
	}
	function toggleTheme() {
		dark = !dark;
		document.documentElement.dataset.theme = dark ? 'dark' : 'light';
		store('theme', dark ? 'dark' : 'light');
	}
	async function signOut() {
		await post('/auth/signout');
		live.stop();
		await live.checkSession();
	}
</script>

{#snippet group(title: string, items: Item[])}
	{@const shut = !rail && !!folded[title]}
	{@const waiting = items.reduce((n, i) => n + (i.meta ?? 0), 0)}
	<div class="sg">
		{#if rail}<span class="sg-rule"></span>{:else}
			<button class="sg-h" class:shut aria-expanded={!shut} onclick={() => fold(title)}>
				<span>{title}</span>
				{#if shut && waiting}<span class="meta {items.some((i) => i.meta && i.sev === 'critical') ? 'critical' : 'warning'}" title="{waiting} need a look">{waiting}</span>{/if}
				<Icon name="chevron" size={13} />
			</button>
		{/if}
		{#each items.filter((i) => !shut || isOn(i)) as i (i.href)}
			<a class="sl" class:on={isOn(i)} href={i.href} title={rail ? i.label + (i.tip ? ' · ' + i.tip : '') : undefined} aria-current={isOn(i) ? 'page' : undefined}>
				<Icon name={i.icon} size={17} /><span class="lbl">{i.label}</span>
				{#if i.meta}<span class="meta {i.sev}" title={i.tip}>{i.meta}</span>{/if}
			</a>
		{/each}
	</div>
{/snippet}

<svelte:window onclick={() => (menu = false)} />

{#if live.session?.door}
	<div class="door" role="alert">Emergency door: signed in through the SSH tunnel. Every channel was told; sign out when you're done.</div>
{/if}
<header class="top">
	<button class="iconbtn menu-btn" aria-label="Open navigation" onclick={(e) => { e.stopPropagation(); drawer = !drawer; }}><Icon name="menu" /></button>
	<a class="logo" href="/"><Mark /><b>Cairn OBS</b></a>
	<span class="live" class:stale={!!live.error} title={live.error || 'Updates every 5 seconds'}>
		<i></i>{live.error ? 'Can’t reach obs-beacond' : live.updated ? 'Live · ' + hms(live.updated) + ' ' + zone(live.updated) : 'Connecting…'}
	</span>
	<div class="right">
		{#if live.session?.dev}<span class="dev-badge" title="Signed in with development sign-in (loopback only)">Dev sign-in</span>{/if}
		{#if live.session?.demo}<span class="dev-badge demo-badge" title="A simulated fleet. You see what an admin sees, and nothing you do changes it.">Demo</span>{/if}
		<button class="iconbtn" aria-label="Switch theme" onclick={toggleTheme}><Icon name={dark ? 'sun' : 'moon'} /></button>
		<button class="iconbtn" aria-label="Account menu" aria-expanded={menu} onclick={(e) => { e.stopPropagation(); menu = !menu; }}><Icon name="user" /></button>
		{#if menu}
			<div class="menu" role="menu">
				<div class="who"><b>{live.session?.user}</b><small>{live.session?.dev ? 'development sign-in' : `via ${live.session?.provider} · ${live.session?.role}`} · Cairn OBS {live.session?.version}</small></div>
				<button role="menuitem" onclick={signOut}><Icon name="logout" />Sign out</button>
			</div>
		{/if}
	</div>
</header>

<div class="layout" class:rail style="--side-w:{width}px" class:resizing>
	<aside class="side" class:rail class:open={drawer} aria-label="Sections">
		<nav class="side-scroll">
			{#if admin}
				<div class="mode" class:rail role="group" aria-label="Side">
					<a class:on={!configuring} href={lastWatch} title={rail ? 'Monitor' : undefined} aria-current={!configuring ? 'true' : undefined}><Icon name="eye" size={15} /><span class="lbl">Monitor</span></a>
					<a class:on={configuring} href={lastConfigure} title={rail ? 'Configure' : undefined} aria-current={configuring ? 'true' : undefined}><Icon name="settings" size={15} /><span class="lbl">Configure</span></a>
				</div>
			{/if}
			{#if configuring}
				{@render group('Configure', setup)}
				{@render group('Access', access)}
			{:else}
				{@render group('Overview', overview)}
				{#if infra.length}{@render group('Infrastructure', infra)}{/if}
				{@render group('Watch', watch)}
			{/if}
		</nav>
		<div class="side-foot">
			<button class="sl" onclick={() => setRail(!rail)} aria-label={rail ? 'Expand navigation' : 'Collapse navigation'}>
				<Icon name={rail ? 'expand' : 'collapse'} size={17} /><span class="lbl">Collapse</span>
			</button>
		</div>
		<!-- A focusable separator is ARIA's window-splitter pattern: drag it, or use the arrow keys. -->
		<!-- svelte-ignore a11y_no_noninteractive_tabindex, a11y_no_noninteractive_element_interactions -->
		<div class="resize" role="separator" aria-orientation="vertical" aria-label="Resize navigation" tabindex="0"
			aria-valuemin={72} aria-valuemax={SIDE_MAX} aria-valuenow={rail ? 72 : width}
			title="Drag to resize · double-click to reset"
			onpointerdown={startResize} onkeydown={resizeKey}
			ondblclick={() => { setRail(false); setWidth(SIDE_DEFAULT, true); }}></div>
	</aside>
	{#if drawer}<button class="back" aria-label="Close navigation" onclick={() => (drawer = false)}></button>{/if}
	<main class="page">{@render children()}</main>
</div>

<style>
	.top { position: sticky; top: 0; z-index: 40; height: 56px; display: flex; align-items: center; gap: 16px; padding: 0 16px; background: var(--background); border-bottom: 1px solid var(--border); }
	.logo { display: flex; align-items: center; gap: 8px; color: var(--foreground); flex: none; }
	.logo b { font-size: 25px; font-weight: 700; letter-spacing: -.03em; line-height: 1; }
	.live { display: inline-flex; align-items: center; gap: 7px; font: 600 10px var(--mono); letter-spacing: .14em; text-transform: uppercase; color: var(--primary); }
	.live i { position: relative; width: 8px; height: 8px; border-radius: 50%; background: currentColor; }
	.live i::after { content: ''; position: absolute; inset: 0; border-radius: 50%; background: currentColor; animation: ping 1.6s cubic-bezier(0, 0, .2, 1) infinite; }
	.live.stale { color: var(--critical); }
	.live.stale i::after { animation: none; }
	@keyframes ping { 75%, 100% { transform: scale(2.4); opacity: 0; } }
	.right { margin-left: auto; display: flex; align-items: center; gap: 8px; position: relative; }
	.dev-badge { font: 600 10px var(--mono); letter-spacing: .14em; text-transform: uppercase; color: var(--warning); border: 1px solid color-mix(in oklab, var(--warning) 50%, transparent); padding: 3px 8px; border-radius: 20px; white-space: nowrap; }
	.demo-badge { color: var(--accent-fg); border-color: color-mix(in oklab, var(--primary) 50%, transparent); }
	.menu { position: absolute; right: 0; top: 46px; width: 240px; background: var(--popover); border: 1px solid var(--border); border-radius: var(--radius); box-shadow: var(--shadow); padding: 6px; z-index: 60; }
	.menu .who { padding: 8px 10px 10px; border-bottom: 1px solid var(--border); margin-bottom: 4px; font-size: 13px; }
	.menu .who small { display: block; color: var(--muted-fg); font-size: 12px; }
	.menu button { display: flex; align-items: center; gap: 8px; width: 100%; padding: 8px 10px; border: 0; border-radius: 4px; background: none; cursor: pointer; font-size: 13px; text-align: left; }
	.menu button:hover { background: var(--muted); }
	.menu-btn { display: none; }

	.layout { display: grid; grid-template-columns: var(--side-w) minmax(0, 1fr); }
	.layout.rail { grid-template-columns: 72px minmax(0, 1fr); }
	.layout.resizing { cursor: col-resize; user-select: none; }
	.side { position: sticky; top: 56px; height: calc(100vh - 56px); display: flex; flex-direction: column; border-right: 1px solid var(--border); background: var(--background); z-index: 30; }
	.side-scroll { flex: 1; overflow-y: auto; padding: 10px 8px; }
	.sg { margin-bottom: 10px; }
	.sg-h { display: flex; align-items: center; gap: 6px; width: 100%; padding: 8px 12px 4px; border: 0; background: none; cursor: pointer; font-size: 11px; font-weight: 600; letter-spacing: .1em; text-transform: uppercase; color: var(--muted-fg); text-align: left; }
	.sg-h > span:first-child { flex: 1; }
	.sg-h:hover { color: var(--foreground); }
	.sg-h :global(svg) { opacity: 0; transition: transform .15s, opacity .15s; }
	.sg-h:hover :global(svg), .sg-h:focus-visible :global(svg), .sg-h.shut :global(svg) { opacity: 1; }
	.sg-h.shut :global(svg) { transform: rotate(-90deg); }
	.sg-h .meta { letter-spacing: 0; }
	.sg-rule { display: block; height: 1px; margin: 6px 14px 8px; background: var(--border); }
	.sl { display: flex; align-items: center; gap: 10px; width: 100%; height: 34px; padding: 0 12px; border: 0; border-radius: var(--radius); background: none; color: var(--muted-fg); font-size: 13.5px; cursor: pointer; text-align: left; position: relative; }
	.sl:hover { background: var(--muted); color: var(--foreground); }
	.sl.on { background: var(--accent); color: var(--accent-fg); font-weight: 500; }
	.lbl { flex: 1; min-width: 0; overflow: hidden; white-space: nowrap; text-overflow: ellipsis; }
	.meta { min-width: 20px; height: 18px; padding: 0 6px; border-radius: 9px; display: inline-grid; place-items: center; font: 600 10px var(--mono); color: #fff; background: var(--critical); }
	.meta.warning { background: var(--warning); color: var(--on-sev); }
	.mode { display: grid; grid-template-columns: 1fr 1fr; gap: 2px; padding: 3px; margin: 0 4px 12px; border: 1px solid var(--border); border-radius: calc(var(--radius) + 2px); background: var(--muted); }
	.mode a { display: flex; align-items: center; justify-content: center; gap: 6px; height: 28px; border-radius: var(--radius); font-size: 12.5px; font-weight: 500; color: var(--muted-fg); text-decoration: none; min-width: 0; }
	.mode a:hover { color: var(--foreground); }
	.mode a.on { background: var(--background); color: var(--foreground); box-shadow: var(--shadow-soft); }
	.mode .lbl { flex: none; }
	.mode.rail { grid-template-columns: 1fr; margin: 0 6px 10px; }
	.mode.rail .lbl { display: none; }
	.side-foot { border-top: 1px solid var(--border); padding: 8px; }
	.side.rail .sl { justify-content: center; padding: 0; height: 38px; }
	.side.rail .lbl { display: none; }
	.side.rail .meta { position: absolute; top: 3px; right: 9px; min-width: 15px; height: 15px; padding: 0 4px; font-size: 9px; border: 2px solid var(--background); }
	.resize { position: absolute; top: 0; right: -5px; width: 10px; height: 100%; cursor: col-resize; z-index: 5; touch-action: none; }
	.resize::after { content: ''; position: absolute; top: 0; bottom: 0; left: 4px; width: 2px; transition: background .15s; }
	.resize:hover::after, .resize:focus-visible::after, .resizing .resize::after { background: var(--primary); }
	.resize:focus-visible { outline: none; }
	.back { display: none; }

	@media (max-width: 860px) {
		.menu-btn { display: grid; }
		.live { display: none; }
		.layout, .layout.rail { grid-template-columns: minmax(0, 1fr); }
		.side, .side.rail { position: fixed; left: 0; top: 56px; bottom: 0; height: auto; width: 260px; transform: translateX(-105%); transition: transform .2s; box-shadow: var(--shadow); }
		.side.open { transform: none; }
		.side.rail .sl { justify-content: flex-start; padding: 0 12px; }
		.side.rail .lbl { display: block; }
		.side.rail .meta { position: static; }
		.resize, .side-foot { display: none; }
		.back { display: block; position: fixed; inset: 56px 0 0; background: rgb(0 0 0 / .4); z-index: 25; border: 0; }
	}
	.door { background: var(--critical); color: #fff; font-weight: 600; font-size: 13px; text-align: center; padding: 6px 16px; }
</style>
