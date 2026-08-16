<script lang="ts">
	// Renders the tab list only -- the caller renders each panel's content
	// (conditionally, on `active`) and is responsible for
	// id="panel-{tab.id}" aria-labelledby="tab-{tab.id}" on it. Keeps this
	// component decoupled from what a panel's content actually is.
	type Tab = { id: string; label: string };

	let {
		tabs,
		active = $bindable('')
	}: {
		tabs: Tab[];
		active?: string;
	} = $props();

	$effect(() => {
		if (!active && tabs.length) active = tabs[0].id;
	});

	function onKeydown(e: KeyboardEvent) {
		const i = tabs.findIndex((t) => t.id === active);
		if (e.key === 'ArrowRight') {
			active = tabs[(i + 1) % tabs.length].id;
			e.preventDefault();
		} else if (e.key === 'ArrowLeft') {
			active = tabs[(i - 1 + tabs.length) % tabs.length].id;
			e.preventDefault();
		}
	}
</script>

<div role="tablist" tabindex="-1" class="tablist" onkeydown={onKeydown}>
	{#each tabs as tab (tab.id)}
		<button
			role="tab"
			id="tab-{tab.id}"
			aria-selected={active === tab.id}
			aria-controls="panel-{tab.id}"
			tabindex={active === tab.id ? 0 : -1}
			class="tab"
			class:active={active === tab.id}
			onclick={() => (active = tab.id)}
		>
			{tab.label}
		</button>
	{/each}
</div>

<style>
	.tablist {
		display: flex;
		gap: var(--space-1);
		border-bottom: 1px solid var(--color-border);
	}
	.tab {
		background: none;
		border: none;
		border-bottom: 2px solid transparent;
		color: var(--color-text-muted);
		font-family: var(--font-ui);
		font-size: var(--text-base);
		font-weight: var(--font-weight-medium);
		padding: var(--space-2) var(--space-3);
		cursor: pointer;
		margin-bottom: -1px;
	}
	.tab:hover {
		color: var(--color-text);
	}
	.tab.active {
		color: var(--color-text);
		border-bottom-color: var(--color-accent);
	}
</style>
