<script lang="ts">
	import Icon from '$lib/Icon.svelte';
	import Panel from '$lib/Panel.svelte';
	import { get, SignedOut, type Audit } from '$lib/api';
	import { live } from '$lib/live.svelte';
	import { ago, hmz, utc } from '$lib/format';

	let data = $state<Audit | null>(null);
	let error = $state('');

	async function load() {
		try {
			data = await get<Audit>('/api/v1/audit');
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
	const day = new Intl.DateTimeFormat('en-US', { month: 'short', day: 'numeric' });
</script>

<svelte:head><title>Cairn OBS · Audit log</title></svelte:head>

<div class="phead">
	<span class="tile"><Icon name="list" /></span>
	<div>
		<h1>Audit log</h1>
		<p>Who signed in, who was turned away, who acknowledged what and when Cairn OBS's setup changed. Newest first.</p>
	</div>
</div>

{#if error}<p class="foot-note" style="color:var(--critical)">{error}</p>{/if}

{#if data}
	<Panel title="Recent" aside="last {data.entries.length}" flush>
		<div class="tbl-wrap flat">
			<table class="tbl">
				<thead><tr><th>When</th><th>Who</th><th>What</th></tr></thead>
				<tbody>
					{#each data.entries as e, n (n)}
						<tr>
							<td class="sub nowrap" title="{utc(e.at)} · {ago(e.at)}">{day.format(new Date(e.at))} {hmz(e.at)}</td>
							<td>{e.who}</td>
							<td>{e.action}{#if e.detail}<div class="sub">{e.detail}</div>{/if}</td>
						</tr>
					{:else}
						<tr><td colspan="3" class="empty">Nothing yet.</td></tr>
					{/each}
				</tbody>
			</table>
		</div>
	</Panel>
	{#if !data.stored}
		<p class="foot-note">obs-beacond has no database configured, so this log isn't kept: it only goes to obs-beacond's own log. Set <span class="mono">database</span> and <span class="mono">state_key_file</span> in obs-beacond.yaml.</p>
	{/if}
{:else}
	<p class="empty">Loading…</p>
{/if}

<style>
	.flat { border: 0; border-radius: 0; }
	.nowrap { white-space: nowrap; }
	:global(.panel) .empty { padding: 20px; }
</style>
