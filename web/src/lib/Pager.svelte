<script lang="ts">
	/** Which slice of a long list is showing, and buttons to the next and previous.
	 * Nothing when the list is empty: the table above says so, once (#44). */
	let { offset = $bindable(0), limit, total, what }: { offset: number; limit: number; total: number; what: string } = $props();
</script>

<div class="pager">
	<span>{total ? `${offset + 1}–${Math.min(offset + limit, total)} of ${total} ${what}` : ''}</span>
	{#if total > limit}
		<button class="btn sm" disabled={offset === 0} onclick={() => (offset = Math.max(0, offset - limit))}>Previous</button>
		<button class="btn sm" disabled={offset + limit >= total} onclick={() => (offset += limit)}>Next</button>
	{/if}
</div>

<style>
	.pager { display: flex; align-items: center; gap: 8px; margin-top: 10px; font-size: 12px; color: var(--muted-fg); flex-wrap: wrap; }
	.pager span { margin-right: auto; }
</style>
