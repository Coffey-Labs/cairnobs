<script lang="ts">
	import Icon from './Icon.svelte';
	import type { Severity } from './api';
	const WORDS: Record<Severity, string> = { good: 'OK', warning: 'Warning', serious: 'Serious', critical: 'Critical', off: 'Off', waiting: 'Waiting' };
	let { s, word = '' }: { s: Severity; word?: string } = $props();
</script>

<span class="st {s}"><Icon name={s === 'waiting' ? 'waiting' : s} size={15} />{word || WORDS[s]}</span>

<style>
	.st { display: inline-flex; align-items: center; gap: 6px; font-size: 13px; font-weight: 500; white-space: nowrap; }
	.good { --c: var(--good); } .warning { --c: var(--warning); } .serious { --c: var(--serious); }
	.critical { --c: var(--critical); } .off, .waiting { --c: var(--muted-fg); }
	.st :global(svg) { color: var(--c); }
</style>
