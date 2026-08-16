<script lang="ts">
	import { goto } from '$app/navigation';
	import { listDashboards, listRules } from '$lib/api';

	let { open = $bindable(false) }: { open?: boolean } = $props();

	type Item = { id: string; label: string; hint: string; go: () => void };

	const staticItems: Item[] = [
		{ id: 'nav-search', label: 'Search', hint: 'Go to', go: () => goto('/') },
		{ id: 'nav-dashboards', label: 'Dashboards', hint: 'Go to', go: () => goto('/dashboards') },
		{ id: 'nav-alerts', label: 'Alerts', hint: 'Go to', go: () => goto('/alerts') },
		{ id: 'nav-data-sources', label: 'Data Sources', hint: 'Go to', go: () => goto('/data-sources') },
		{ id: 'nav-settings', label: 'Settings', hint: 'Go to', go: () => goto('/settings') }
	];

	let dynamicItems: Item[] = $state([]);
	let loaded = false;
	let query = $state('');
	let selected = $state(0);
	let inputEl: HTMLInputElement | undefined = $state();
	let dialogEl: HTMLDialogElement | undefined = $state();

	async function loadDynamic() {
		if (loaded) return;
		loaded = true;
		const [dashboards, rules] = await Promise.allSettled([listDashboards(), listRules()]);
		const items: Item[] = [];
		if (dashboards.status === 'fulfilled') {
			for (const d of dashboards.value) {
				items.push({
					id: `dash-${d.id}`,
					label: d.name,
					hint: 'Dashboard',
					go: () => goto(`/dashboards/${d.id}`)
				});
			}
		}
		if (rules.status === 'fulfilled') {
			for (const r of rules.value) {
				items.push({
					id: `rule-${r.id}`,
					label: r.name,
					hint: 'Alert rule',
					go: () => goto(`/alerts/${r.id}`)
				});
			}
		}
		dynamicItems = items;
	}

	let allItems = $derived([...staticItems, ...dynamicItems]);
	let filtered = $derived(
		query.trim() === ''
			? allItems
			: allItems.filter((i) => i.label.toLowerCase().includes(query.trim().toLowerCase()))
	);

	$effect(() => {
		selected = 0;
	});

	// Native <dialog> instead of a hand-rolled overlay -- same reasoning
	// as ui/Modal.svelte: focus trap, Escape-to-close, and top-layer
	// stacking come for free, and it resolves the a11y-linter warnings a
	// plain role="dialog" div on a non-interactive element raises,
	// rather than suppressing them.
	$effect(() => {
		if (!dialogEl) return;
		if (open && !dialogEl.open) {
			dialogEl.showModal();
			loadDynamic();
			queueMicrotask(() => inputEl?.focus());
		}
		if (!open && dialogEl.open) dialogEl.close();
	});

	function onDialogClose() {
		open = false;
		query = '';
	}

	function onBackdropClick(e: MouseEvent) {
		if (e.target === dialogEl) open = false;
	}

	function choose(item: Item) {
		item.go();
		open = false;
	}

	function onKeydown(e: KeyboardEvent) {
		if (e.key === 'ArrowDown') {
			selected = Math.min(selected + 1, filtered.length - 1);
			e.preventDefault();
		} else if (e.key === 'ArrowUp') {
			selected = Math.max(selected - 1, 0);
			e.preventDefault();
		} else if (e.key === 'Enter') {
			if (filtered[selected]) choose(filtered[selected]);
			e.preventDefault();
		}
	}

	function onGlobalKeydown(e: KeyboardEvent) {
		const isK = e.key === 'k' || e.key === 'K';
		if (isK && (e.metaKey || e.ctrlKey)) {
			e.preventDefault();
			open = !open;
		}
	}
</script>

<svelte:window onkeydown={onGlobalKeydown} />

<dialog bind:this={dialogEl} class="palette" onclose={onDialogClose} onclick={onBackdropClick} aria-label="Command palette">
	<div class="frame">
		<input
			bind:this={inputEl}
			bind:value={query}
			onkeydown={onKeydown}
			type="text"
			placeholder="Jump to a dashboard, alert rule, or page…"
			aria-label="Search"
			role="combobox"
			aria-expanded="true"
			aria-controls="palette-listbox"
		/>
		<ul id="palette-listbox" role="listbox">
			{#each filtered as item, i (item.id)}
				<li role="option" aria-selected={i === selected}>
					<button type="button" class:selected={i === selected} onclick={() => choose(item)}>
						<span class="label">{item.label}</span>
						<span class="hint">{item.hint}</span>
					</button>
				</li>
			{:else}
				<li class="empty">No matches</li>
			{/each}
		</ul>
	</div>
</dialog>

<style>
	.palette {
		position: fixed;
		top: 12vh;
		left: 0;
		right: 0;
		margin: 0 auto;
		padding: 0;
		width: min(34rem, calc(100vw - 2rem));
		background: var(--color-surface);
		border: 1px solid var(--color-border);
		border-radius: var(--radius-lg);
		box-shadow: var(--shadow-lg);
		overflow: hidden;
	}
	.palette::backdrop {
		background: rgba(0, 0, 0, 0.55);
	}
	.frame {
		display: flex;
		flex-direction: column;
	}
	input {
		width: 100%;
		height: 3rem;
		padding: 0 var(--space-4);
		border: none;
		border-bottom: 1px solid var(--color-border);
		background: transparent;
		color: var(--color-text);
		font-family: var(--font-ui);
		font-size: var(--text-md);
	}
	input:focus {
		outline: none;
	}
	input::placeholder {
		color: var(--color-text-faint);
	}
	ul {
		list-style: none;
		margin: 0;
		padding: var(--space-2);
		max-height: 20rem;
		overflow-y: auto;
	}
	li {
		margin: 0;
	}
	li.empty {
		padding: var(--space-3);
		color: var(--color-text-muted);
		font-size: var(--text-sm);
	}
	button {
		width: 100%;
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-3);
		background: none;
		border: none;
		border-radius: var(--radius-sm);
		padding: var(--space-2) var(--space-3);
		color: var(--color-text);
		font-family: var(--font-ui);
		font-size: var(--text-base);
		text-align: left;
		cursor: pointer;
	}
	button.selected {
		background: var(--color-surface-raised);
	}
	.hint {
		font-size: var(--text-xs);
		color: var(--color-text-muted);
		font-family: var(--font-mono);
	}
</style>
