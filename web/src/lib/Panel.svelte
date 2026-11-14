<script lang="ts">
	import type { Snippet } from 'svelte';
	let { title = '', aside = '', href = '', hrefLabel = '', flush = false, children }:
		{ title?: string; aside?: string; href?: string; hrefLabel?: string; flush?: boolean; children: Snippet } = $props();
</script>

<section class="panel" class:flush>
	<span class="crn tl"></span><span class="crn tr"></span><span class="crn bl"></span><span class="crn br"></span>
	{#if title || aside || href}
	<header>
		<h2>{title}</h2>
		<span class="rule"></span>
		{#if aside}<span class="aside">{aside}</span>{/if}
		{#if href}<a class="go" {href}>{hrefLabel} →</a>{/if}
	</header>
	{/if}
	{@render children()}
</section>

<style>
	.panel { position: relative; display: flex; flex-direction: column; border: 1px solid var(--border); border-radius: var(--radius); background: var(--card); box-shadow: var(--shadow-soft); padding: 20px; min-width: 0; }
	.panel.flush { padding: 0; }
	.crn { position: absolute; width: 10px; height: 10px; border: 0 solid color-mix(in oklab, var(--primary) 50%, transparent); pointer-events: none; }
	.tl { left: -1px; top: -1px; border-left-width: 2px; border-top-width: 2px; border-top-left-radius: var(--radius); }
	.tr { right: -1px; top: -1px; border-right-width: 2px; border-top-width: 2px; border-top-right-radius: var(--radius); }
	.bl { left: -1px; bottom: -1px; border-left-width: 2px; border-bottom-width: 2px; border-bottom-left-radius: var(--radius); }
	.br { right: -1px; bottom: -1px; border-right-width: 2px; border-bottom-width: 2px; border-bottom-right-radius: var(--radius); }
	header { display: flex; align-items: center; gap: 12px; margin-bottom: 16px; flex-wrap: wrap; }
	.flush header { padding: 18px 20px 0; margin-bottom: 12px; }
	h2 { font-size: 11px; font-weight: 600; letter-spacing: .16em; text-transform: uppercase; color: var(--muted-fg); white-space: nowrap; }
	.rule { height: 1px; flex: 1; min-width: 20px; background: linear-gradient(90deg, var(--border), transparent); }
	.aside { font: 600 10px var(--mono); letter-spacing: .14em; text-transform: uppercase; color: var(--muted-fg); }
	.go { font-size: 12px; font-weight: 500; white-space: nowrap; }
</style>
