<script lang="ts">
	// Export and import for a settings section edited in the UI (checks,
	// maintenance). Export downloads the YAML its file would hold; import
	// replaces the whole section, after asking.
	import Icon from '$lib/Icon.svelte';
	import { sendYAML } from '$lib/api';

	let { section, what, count, editable, onchange }:
		{ section: string; what: string; count: number; editable: boolean; onchange: () => void } = $props();

	let picker = $state<HTMLInputElement>();
	let pending = $state<{ name: string; text: string } | null>(null);
	let error = $state('');
	let busy = $state(false);

	async function picked() {
		const f = picker?.files?.[0];
		if (!f) return;
		error = '';
		pending = { name: f.name, text: await f.text() };
		if (picker) picker.value = '';
	}
	async function doImport() {
		if (!pending) return;
		busy = true;
		try {
			await sendYAML(`/api/v1/settings/${section}/import`, pending.text);
			pending = null;
			onchange();
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		} finally {
			busy = false;
		}
	}
</script>

<a class="btn sm" href="/api/v1/settings/{section}/export" download="{section}.yaml"><Icon name="download" /> Export YAML</a>
{#if editable}
	<button class="btn sm" onclick={() => picker?.click()}><Icon name="upload" /> Import YAML</button>
	<input type="file" accept=".yaml,.yml,application/yaml,text/yaml" hidden bind:this={picker} onchange={picked} />
{/if}
{#if pending}
	<div class="ask" role="alertdialog" aria-label="Import {what}">
		<span>{#if count}Replace {count === 1 ? `the one ${what.replace(/s$/, '')}` : `all ${count} ${what}`} with{:else}Import{/if} <b class="mono">{pending.name}</b>?</span>
		<button class="btn sm pri" disabled={busy} onclick={doImport}>{count ? 'Replace' : 'Import'}</button>
		<button class="btn sm" onclick={() => { pending = null; error = ''; }}>Cancel</button>
		{#if error}<span class="err">{error}</span>{/if}
	</div>
{/if}

<style>
	.sm { height: 30px; padding: 0 12px; font-size: 13px; text-decoration: none; }
	.ask { flex-basis: 100%; display: flex; flex-wrap: wrap; gap: 8px; align-items: center; justify-content: flex-end; font-size: 13px; }
	.err { flex-basis: 100%; text-align: right; color: var(--critical); }
</style>
