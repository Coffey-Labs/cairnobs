<script lang="ts">
	import { paths, type IconName } from './icons';
	let { name, size = 16, style = '' }: { name: IconName; size?: number; style?: string } = $props();
</script>

<svg class="i" width={size} height={size} {style} viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
	{@html paths[name]}
</svg>
