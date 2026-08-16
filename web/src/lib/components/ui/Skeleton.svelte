<script lang="ts">
	let { width = '100%', height = '1rem' }: { width?: string; height?: string } = $props();
</script>

<span class="skeleton" style:width style:height></span>

<style>
	.skeleton {
		display: block;
		border-radius: var(--radius-sm);
		background: linear-gradient(
			90deg,
			var(--color-surface-raised) 25%,
			var(--color-border) 50%,
			var(--color-surface-raised) 75%
		);
		background-size: 200% 100%;
		animation: shimmer 1.4s ease-in-out infinite;
	}
	@media (prefers-reduced-motion: reduce) {
		.skeleton {
			animation: none;
			background: var(--color-surface-raised);
		}
	}
	@keyframes shimmer {
		0% {
			background-position: 200% 0;
		}
		100% {
			background-position: -200% 0;
		}
	}
</style>
