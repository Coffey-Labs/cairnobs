<script lang="ts">
	import { getTimezone } from '$lib/timezone.svelte';
	import { relativeTime, formatTimestamp, zoneLabel } from '$lib/time';
	import { page } from '$app/state';
	import { getHostMetrics, type HostMetrics } from '$lib/api';
	import { Card, Skeleton } from '$lib/components/ui';

	const host = page.params.host!;

	let metrics = $state<HostMetrics | null>(null);
	let loading = $state(true);
	let error = $state('');

	async function load() {
		loading = true;
		error = '';
		try {
			metrics = await getHostMetrics(host);
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		} finally {
			loading = false;
		}
	}
	load();

	function formatBytes(bytes: number): string {
		if (bytes <= 0) return '0 B';
		const units = ['B', 'KB', 'MB', 'GB', 'TB'];
		const i = Math.min(units.length - 1, Math.floor(Math.log(bytes) / Math.log(1024)));
		return `${(bytes / 1024 ** i).toFixed(1)} ${units[i]}`;
	}

	function percent(used: number, total: number): number {
		if (total <= 0) return 0;
		return Math.min(100, Math.max(0, (used / total) * 100));
	}


	function formatUptime(seconds: number): string {
		if (seconds <= 0) return '—';
		const days = Math.floor(seconds / 86400);
		const hours = Math.floor((seconds % 86400) / 3600);
		const minutes = Math.floor((seconds % 3600) / 60);
		if (days > 0) return `${days}d ${hours}h`;
		if (hours > 0) return `${hours}h ${minutes}m`;
		return `${minutes}m`;
	}
</script>

<main>
	<a class="back" href="/hosts">← Hosts</a>
	<h1>{host}</h1>

	{#if loading}
		<Skeleton height="12rem" />
	{:else if error}
		<p class="error">Error: {error}</p>
	{:else if !metrics}
		<p class="hint">No metrics samples for this host yet.</p>
	{:else}
		<p class="hint" title={`${formatTimestamp(metrics.timestamp, getTimezone())} ${zoneLabel(getTimezone())}`}>Last sample {relativeTime(metrics.timestamp)}.</p>

		<section class="system">
			<dl>
				<dt>OS</dt>
				<dd>{metrics.osName}</dd>
				<dt>Kernel</dt>
				<dd>{metrics.kernelVersion}</dd>
				<dt>Architecture</dt>
				<dd>{metrics.arch}</dd>
				<dt>Uptime</dt>
				<dd>{formatUptime(metrics.uptimeSeconds)}</dd>
				<dt>IPv4</dt>
				<dd>{metrics.ipv4Addresses.length > 0 ? metrics.ipv4Addresses.join(', ') : '—'}</dd>
				<dt>IPv6</dt>
				<dd>{metrics.ipv6Addresses.length > 0 ? metrics.ipv6Addresses.join(', ') : '—'}</dd>
			</dl>
		</section>

		<div class="stats">
			<Card title="CPU">
				<div class="big-number">{metrics.cpuPercent.toFixed(1)}%</div>
				<div class="bar">
					<div class="bar-fill" style="width: {metrics.cpuPercent.toFixed(1)}%"></div>
				</div>
				<div class="detail">{metrics.cpuCores} core{metrics.cpuCores === 1 ? '' : 's'}</div>
			</Card>

			<Card title="Memory">
				<div class="big-number">{percent(metrics.memUsedBytes, metrics.memTotalBytes).toFixed(1)}%</div>
				<div class="bar">
					<div
						class="bar-fill"
						style="width: {percent(metrics.memUsedBytes, metrics.memTotalBytes).toFixed(1)}%"
					></div>
				</div>
				<div class="detail">{formatBytes(metrics.memUsedBytes)} / {formatBytes(metrics.memTotalBytes)}</div>
			</Card>

			<Card title="Disk (/)">
				<div class="big-number">{percent(metrics.diskUsedBytes, metrics.diskTotalBytes).toFixed(1)}%</div>
				<div class="bar">
					<div
						class="bar-fill"
						style="width: {percent(metrics.diskUsedBytes, metrics.diskTotalBytes).toFixed(1)}%"
					></div>
				</div>
				<div class="detail">{formatBytes(metrics.diskUsedBytes)} / {formatBytes(metrics.diskTotalBytes)}</div>
			</Card>
		</div>
	{/if}
</main>

<style>
	main {
		max-width: 48rem;
	}
	.back {
		font-size: var(--text-sm);
		color: var(--color-text-muted);
		text-decoration: none;
	}
	.back:hover {
		color: var(--color-accent);
	}
	h1 {
		font-size: var(--text-xl);
		margin: var(--space-2) 0 var(--space-2);
		font-family: var(--font-mono);
	}
	.hint {
		color: var(--color-text-muted);
		font-size: var(--text-sm);
		margin-bottom: var(--space-5);
	}
	.error {
		color: var(--color-danger);
	}
	.system {
		margin-bottom: var(--space-5);
	}
	.system dl {
		display: grid;
		grid-template-columns: auto 1fr;
		gap: var(--space-1) var(--space-4);
		font-size: var(--text-sm);
	}
	.system dt {
		color: var(--color-text-muted);
	}
	.system dd {
		margin: 0;
	}
	.stats {
		display: grid;
		grid-template-columns: repeat(auto-fit, minmax(14rem, 1fr));
		gap: var(--space-4);
	}
	.big-number {
		font-size: var(--text-xl);
		font-weight: var(--font-weight-bold);
		margin-bottom: var(--space-3);
	}
	.bar {
		height: 0.5rem;
		border-radius: var(--radius-sm);
		background: var(--color-bg);
		border: 1px solid var(--color-border);
		overflow: hidden;
	}
	.bar-fill {
		height: 100%;
		background: var(--color-accent);
	}
	.detail {
		margin-top: var(--space-2);
		font-size: var(--text-sm);
		color: var(--color-text-muted);
	}
</style>
