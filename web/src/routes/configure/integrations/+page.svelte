<script lang="ts">
	// What Cairn OBS can watch beyond the hosts' own reports, and whether each
	// is set up. They're set in obs-beacond.yaml for now; setting them here
	// comes with the add-ons.
	import Icon from '$lib/Icon.svelte';
	import Panel from '$lib/Panel.svelte';
	import Status from '$lib/Status.svelte';
	import { get, SignedOut, type SettingsSummary } from '$lib/api';
	import { live } from '$lib/live.svelte';

	let s = $state<SettingsSummary | null>(null);
	let error = $state('');
	$effect(() => {
		get<SettingsSummary>('/api/v1/settings').then((r) => { s = r; error = ''; }).catch((e) => {
			if (e instanceof SignedOut) void live.checkSession();
			else error = String(e);
		});
	});

	type Item = { key: keyof SettingsSummary['integrations']; name: string; icon: 'cloud' | 'mail' | 'database' | 'archive' | 'network' | 'report'; page?: string; what: string; how: string; file?: string };
	const items: Item[] = [
		{ key: 'topology', name: 'Maps', icon: 'network', page: '/map/network', what: 'The network and service maps: which host talks to which, and what depends on what.', how: 'topology', file: 'topology.yaml' },
		{ key: 'mail', name: 'Mail', icon: 'mail', page: '/mail', what: 'A monitor account sends itself mail and reads it back over IMAP and JMAP, so you know mail really flows.', how: 'mail' },
		{ key: 'datalayer', name: 'Data layer', icon: 'database', page: '/data', what: 'A Patroni (PostgreSQL) cluster: the leader, replication lag, the synchronous standby and status probes.', how: 'datalayer' },
		{ key: 'backups', name: 'Backups', icon: 'archive', page: '/backups', what: 'Whether every host’s backups ran, and how old the newest one is.', how: 'backups', file: 'backups.yaml' },
		{ key: 'cloudflare', name: 'Cloudflare', icon: 'cloud', page: '/cloudflare', what: 'Zones, DNS, load balancers and origin health, with a read-only API token.', how: 'cloudflare' },
		{ key: 'digest', name: 'Weekly digest', icon: 'report', page: '/reports', what: 'The last seven days, summed up and sent to your alert channels once a week.', how: 'reports.digest' }
	];
</script>

<svelte:head><title>Cairn OBS · Integrations</title></svelte:head>

<div class="phead">
	<span class="tile"><Icon name="plug" /></span>
	<div>
		<h1>Integrations</h1>
		<p>What Cairn OBS watches beyond each host's own report. Each one set up adds its page to the monitoring side. They're set in obs-beacond.yaml for now (docs/formats.md); setting them up here is coming.</p>
	</div>
</div>

{#if error}<p class="foot-note" style="color:var(--critical)">{error}</p>{/if}

{#if s}
	<Panel flush>
		<ul class="list">
			{#each items as it (it.key)}
				{@const set = !!s.integrations[it.key]}
				<li>
					<span class="ic"><Icon name={it.icon} /></span>
					<div class="body">
						<div class="nm">{it.name}{#if set && it.page}{' '}<a href={it.page}>Open</a>{/if}</div>
						<p class="sub">{it.what}</p>
						{#if !set}<p class="sub how">To set it up, {#if it.file}point <span class="mono">{it.how}</span> in obs-beacond.yaml at a {it.file}{:else}add a <span class="mono">{it.how}</span> section to obs-beacond.yaml{/if}.</p>{/if}
					</div>
					{#if set}<Status s="good" word="Set up" />{:else}<Status s="off" word="Not set up" />{/if}
				</li>
			{/each}
		</ul>
	</Panel>
{:else}
	<p class="empty">Loading…</p>
{/if}

<style>
	.list { list-style: none; margin: 0; padding: 0; }
	.list li { display: flex; gap: 14px; align-items: flex-start; padding: 16px 20px; border-top: 1px solid var(--border); }
	.list li:first-child { border-top: 0; }
	.ic { width: 34px; height: 34px; border-radius: var(--radius); display: grid; place-items: center; background: var(--muted); color: var(--muted-fg); flex: none; }
	.ic :global(svg) { width: 18px; height: 18px; }
	.body { flex: 1; min-width: 0; }
	.nm { font-weight: 600; }
	.nm a { font-weight: 500; font-size: 12.5px; margin-left: 6px; }
	.sub { margin: 2px 0 0; font-size: 13px; }
	.how { margin-top: 6px; }
</style>
