<script lang="ts">
	import type { Snippet } from 'svelte';

	// Built on <dialog> deliberately -- native focus trap, Escape-to-close,
	// and top-layer stacking for free, instead of hand-rolling all three.
	let {
		open = $bindable(false),
		title,
		children,
		footer
	}: {
		open?: boolean;
		title: string;
		children: Snippet;
		footer?: Snippet;
	} = $props();

	let dialogEl: HTMLDialogElement | undefined = $state();

	$effect(() => {
		if (!dialogEl) return;
		if (open && !dialogEl.open) dialogEl.showModal();
		if (!open && dialogEl.open) dialogEl.close();
	});

	function onClose() {
		open = false;
	}

	function onBackdropClick(e: MouseEvent) {
		if (e.target === dialogEl) open = false;
	}
</script>

<dialog bind:this={dialogEl} onclose={onClose} onclick={onBackdropClick} aria-labelledby="modal-title">
	<div class="frame">
		<header>
			<h2 id="modal-title">{title}</h2>
			<button type="button" class="close" onclick={() => (open = false)} aria-label="Close">✕</button>
		</header>
		<div class="content">
			{@render children()}
		</div>
		{#if footer}
			<footer>{@render footer()}</footer>
		{/if}
	</div>
</dialog>

<style>
	dialog {
		padding: 0;
		border: 1px solid var(--color-border);
		border-radius: var(--radius-lg);
		background: var(--color-surface);
		color: var(--color-text);
		box-shadow: var(--shadow-lg);
		width: min(32rem, calc(100vw - 2rem));
	}
	dialog::backdrop {
		background: rgba(0, 0, 0, 0.55);
	}
	.frame {
		display: flex;
		flex-direction: column;
	}
	header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		padding: var(--space-4);
		border-bottom: 1px solid var(--color-border);
	}
	h2 {
		font-size: var(--text-lg);
		font-weight: var(--font-weight-medium);
	}
	.close {
		background: none;
		border: none;
		color: var(--color-text-muted);
		cursor: pointer;
		font-size: var(--text-base);
		line-height: 1;
		padding: var(--space-1);
	}
	.close:hover {
		color: var(--color-text);
	}
	.content {
		padding: var(--space-4);
		overflow-y: auto;
	}
	footer {
		display: flex;
		justify-content: flex-end;
		gap: var(--space-2);
		padding: var(--space-4);
		border-top: 1px solid var(--color-border);
	}
</style>
