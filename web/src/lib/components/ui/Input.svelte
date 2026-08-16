<script lang="ts">
	let {
		value = $bindable(''),
		type = 'text',
		placeholder = '',
		disabled = false,
		id,
		invalid = false,
		...rest
	}: {
		value?: string;
		type?: string;
		placeholder?: string;
		disabled?: boolean;
		id?: string;
		invalid?: boolean;
		[key: string]: unknown;
	} = $props();
</script>

<input
	{id}
	{type}
	{placeholder}
	{disabled}
	bind:value
	aria-invalid={invalid}
	class="input"
	class:invalid
	{...rest}
/>

<style>
	.input {
		height: var(--control-height);
		padding: 0 var(--space-3);
		background: var(--color-surface);
		border: 1px solid var(--color-border);
		border-radius: var(--radius-sm);
		color: var(--color-text);
		font-family: var(--font-ui);
		font-size: var(--text-base);
		width: 100%;
	}
	.input::placeholder {
		color: var(--color-text-faint);
	}
	.input:hover:not(:disabled) {
		border-color: var(--color-border-strong);
	}
	.input:disabled {
		opacity: 0.5;
	}
	.input.invalid {
		border-color: var(--color-danger);
	}
</style>
