<script lang="ts">
	// The control plane's front page: every area that can be set up, where
	// it's set, and how much is in it. The monitoring side shows only what
	// has been set up; this is where the rest is.
	import Icon from '$lib/Icon.svelte';
	import Panel from '$lib/Panel.svelte';
	import { get, SignedOut, type SettingsSummary, type Source } from '$lib/api';
	import { live } from '$lib/live.svelte';

	let s = $state<SettingsSummary | null>(null);
	let error = $state('');

	async function load() {
		try {
			s = await get<SettingsSummary>('/api/v1/settings');
			error = '';
		} catch (e) {
			if (e instanceof SignedOut) void live.checkSession();
			else error = String(e);
		}
	}
	$effect(() => {
		void load();
	});

	const where = (src: Source, file?: string) => (src === 'file' ? `set in ${file || 'obs-beacond.yaml'}` : src === 'ui' ? 'set here' : 'needs a database');
	const n = (k: number, one: string, many: string) => `${k} ${k === 1 ? one : many}`;
	const INTEGRATIONS: Record<string, string> = { cloudflare: 'Cloudflare', mail: 'Mail', datalayer: 'Data layer', backups: 'Backups', topology: 'Maps', digest: 'Weekly digest' };
	const on = $derived(s ? Object.entries(s.integrations).filter(([, v]) => v).map(([k]) => INTEGRATIONS[k] ?? k) : []);
	const SIGNIN: Record<string, string> = { oidc: 'your identity provider', inbuxa: 'inbuxa', passkey: 'passkeys', local: 'accounts here' };
</script>

<svelte:head><title>Cairn OBS · Configure</title></svelte:head>

<div class="phead">
	<span class="tile"><Icon name="settings" /></span>
	<div>
		<h1>Configure</h1>
		<p>What Cairn OBS watches and who it tells. Anything set in obs-beacond.yaml, or in a file it names, wins and shows here read-only; the rest is set here and kept in the database.</p>
	</div>
</div>

{#if error}<p class="foot-note" style="color:var(--critical)">{error}</p>{/if}

{#if s}
	{#if !s.database}
		<p class="note">No database is configured, so nothing can be set here yet: set <span class="mono">database</span> in obs-beacond.yaml, or keep everything in files (docs/formats.md).</p>
	{/if}
	<div class="cards">
		<Panel title="Hosts" href="/configure/hosts" hrefLabel="Open">
			<b class="big">{s.hosts}</b>
			<p class="sub">{n(s.hosts, 'host runs', 'hosts run')} an agent. What should run is chosen on {n(s.sections.expected.count, 'host', 'hosts')}, {where(s.sections.expected.source, s.sections.expected.file)}.</p>
		</Panel>
		<Panel title="Clusters" href="/configure/clusters" hrefLabel="Open">
			<b class="big">{s.clusters}</b>
			<p class="sub">{n(s.clusters, 'cluster runs', 'clusters run')} a cluster agent, which sends its nodes, pods, controllers, jobs and volumes.</p>
		</Panel>
		<Panel title="Checks" href="/configure/checks" hrefLabel="Open">
			<b class="big">{s.sections.checks.count}</b>
			<p class="sub">Websites, ports, certificates, DNS and ping, {where(s.sections.checks.source, s.sections.checks.file)}.</p>
		</Panel>
		<Panel title="Alerts" href="/configure/alerts" hrefLabel="Open">
			<b class="big">{s.sections.channels.count}</b>
			<p class="sub">{n(s.sections.channels.count, 'channel', 'channels')}, {where(s.sections.channels.source)}; {n(s.sections.maintenance.count, 'maintenance window', 'maintenance windows')}, {where(s.sections.maintenance.source)}.</p>
		</Panel>
		<Panel title="Integrations" href="/configure/integrations" hrefLabel="Open">
			<b class="big">{on.length}</b>
			<p class="sub">{on.length ? on.join(', ') : 'None set up yet'}. Each one adds its page to the monitoring side.</p>
		</Panel>
		<Panel title="Users and sign-in" href="/configure/users" hrefLabel="Open">
			<b class="big"><Icon name="users" /></b>
			<p class="sub">Signing in with {s.signin.length ? s.signin.map((t) => SIGNIN[t] ?? t).join(', ') : 'accounts kept here'}. Who may watch, and who may change things.</p>
		</Panel>
		<Panel title="Audit log" href="/configure/audit" hrefLabel="Open">
			<b class="big"><Icon name="list" /></b>
			<p class="sub">Who signed in, who was turned away and every change made here.</p>
		</Panel>
	</div>
{:else}
	<p class="empty">Loading…</p>
{/if}

<style>
	.cards { display: grid; grid-template-columns: repeat(auto-fill, minmax(280px, 1fr)); gap: 16px; }
	.big { display: block; font: 600 28px var(--mono); line-height: 1; margin-bottom: 8px; }
	.big :global(svg) { width: 26px; height: 26px; color: var(--primary); }
	.sub { margin: 0; font-size: 13px; }
	.note { border: 1px solid var(--warning); border-radius: var(--radius); padding: 12px 14px; margin: 0 0 16px; font-size: 13px; }
</style>
