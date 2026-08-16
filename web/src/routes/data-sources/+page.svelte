<script lang="ts">
	import { Card } from '$lib/components/ui';
	import { enterpriseAuthBase } from '$lib/api';
</script>

<main class="page">
	<h1>Data Sources</h1>
	<p class="lede">
		Every tenant has exactly one data source today — its own ClickHouse database and Tantivy
		index, provisioned together when the tenant is created. There's nothing to configure yet.
	</p>

	<Card title="Current data source">
		{#if enterpriseAuthBase}
			<p>
				Your queries run against your tenant's dedicated ClickHouse database and Tantivy index —
				never a shared one. See <a href="/settings">Settings</a> for SSO and tenant configuration.
			</p>
		{:else}
			<p>
				This deployment isn't running in multi-tenant mode, so there's one data source for the
				whole instance — the default ClickHouse database and Tantivy index every query already
				runs against.
			</p>
		{/if}
	</Card>

	<p class="note">
		Multiple data sources per tenant — a second ClickHouse cluster, a read replica, an external
		source — is real future work, not something this page is hiding. The
		<code>data_sources</code> table this reads from was already built with that in mind (see
		<code>/docs/phase-4-rbac-design.md</code>).
	</p>
</main>

<style>
	.page {
		max-width: 42rem;
		display: flex;
		flex-direction: column;
		gap: var(--space-4);
	}
	h1 {
		font-size: var(--text-xl);
	}
	.lede {
		color: var(--color-text-muted);
		margin: 0;
	}
	.note {
		font-size: var(--text-sm);
		color: var(--color-text-muted);
	}
</style>
