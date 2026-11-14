<script lang="ts">
	import { page } from '$app/state';
	import Icon from '$lib/Icon.svelte';
	import Adopt from '$lib/Adopt.svelte';
	import Status from '$lib/Status.svelte';
	import { get, SignedOut, type Host } from '$lib/api';
	import { live } from '$lib/live.svelte';

	const name = $derived(page.params.name ?? '');
	let host = $state<Host | null>(null);
	let missing = $state(false);

	async function load(n: string) {
		try {
			host = (await get<{ host: Host }>('/api/v1/hosts/' + encodeURIComponent(n))).host;
			missing = false;
		} catch (e) {
			if (e instanceof SignedOut) void live.checkSession();
			else missing = String(e).includes(': 404 ');
		}
	}
	$effect(() => {
		const n = name;
		host = null;
		missing = false;
		void load(n);
		const t = setInterval(() => void load(n), 10_000);
		return () => clearInterval(t);
	});
</script>

<svelte:head><title>Cairn OBS · Configure {name}</title></svelte:head>

<p class="crumb"><a href="/configure/hosts">Hosts</a> / <span class="mono">{name}</span></p>
<div class="phead">
	<span class="tile"><Icon name="hosts" /></span>
	<div>
		<h1>{name}</h1>
		<p>What should run here. Cairn OBS checks every report from this host against it, and anything that stops or goes missing alerts.</p>
	</div>
	<div class="acts">
		{#if host}<Status s={host.status} word={host.word} />{/if}
		<a class="btn sm" href="/hosts/{encodeURIComponent(name)}"><Icon name="eye" /> Watch</a>
	</div>
</div>

{#if missing}
	<p class="empty">No host called {name}. <a href="/configure/hosts">Back to the hosts</a>.</p>
{:else}
	<Adopt host={name} onchange={() => { void load(name); void live.refresh(); }} />
{/if}

<style>
	.crumb { margin: 0 0 10px; font-size: 13px; color: var(--muted-fg); }
	.crumb a { color: inherit; }
</style>
