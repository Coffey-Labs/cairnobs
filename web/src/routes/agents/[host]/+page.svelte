<script lang="ts">
	import { page } from '$app/state';
	import { getAgent, setAgentConfig, clearAgentConfig, issueAgentCommand, type Agent } from '$lib/api';
	import { Badge, Button, Input, Skeleton } from '$lib/components/ui';

	const host = page.params.host!;

	let agent = $state<Agent | null>(null);
	let loading = $state(true);
	let error = $state('');
	let saving = $state(false);
	let saveError = $state('');

	// Editable form fields -- seeded from the agent's current
	// desired_override when one exists, otherwise from its currently-
	// reported effective values. Saving always PUTs the complete set
	// (api/agents.Store.SetOverride replaces the whole stored override,
	// it doesn't patch individual fields), so every field needs a
	// sensible starting value regardless of whether an override exists
	// yet -- see /docs/agent-management-design.md. Kept as strings since
	// the shared <Input> component's `value` prop is typed string
	// (native <input type=number>'s bound value is a string too, DOM-
	// side) -- converted to numbers only at save().
	let batchMaxSize = $state('0');
	let batchFlushIntervalMs = $state('0');
	let heartbeatEnabled = $state(true);
	let heartbeatIntervalMs = $state('0');
	let journaldUnit = $state('');
	// Extra file paths to tail in addition to the agent's primary
	// source -- unlike every field above, there's no "effective value"
	// to fall back to when no override exists yet (this is purely a
	// remote-override concept, agent.toml has no equivalent field), so
	// an agent with no override starts with an empty list, not
	// something derived from `agent`.
	let extraFilePaths = $state<string[]>([]);

	function resetForm(a: Agent) {
		const o = a.desired_override;
		batchMaxSize = String(o?.batch_max_size ?? a.batch_max_size);
		batchFlushIntervalMs = String(o?.batch_flush_interval_ms ?? a.batch_flush_interval_ms);
		heartbeatEnabled = o?.heartbeat_enabled ?? a.heartbeat_enabled;
		heartbeatIntervalMs = String(o?.heartbeat_interval_ms ?? a.heartbeat_interval_ms);
		journaldUnit = o?.journald_unit ?? '';
		extraFilePaths = o?.extra_file_paths ? [...o.extra_file_paths] : [];
	}

	function addExtraFilePath() {
		extraFilePaths = [...extraFilePaths, ''];
	}

	function removeExtraFilePath(index: number) {
		extraFilePaths = extraFilePaths.filter((_, i) => i !== index);
	}

	async function load() {
		loading = true;
		error = '';
		try {
			agent = await getAgent(host);
			resetForm(agent);
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		} finally {
			loading = false;
		}
	}
	load();

	async function save() {
		saving = true;
		saveError = '';
		try {
			agent = await setAgentConfig(host, {
				batch_max_size: Number(batchMaxSize),
				batch_flush_interval_ms: Number(batchFlushIntervalMs),
				heartbeat_enabled: heartbeatEnabled,
				heartbeat_interval_ms: Number(heartbeatIntervalMs),
				...(agent?.source_kind === 'journald' ? { journald_unit: journaldUnit } : {}),
				extra_file_paths: extraFilePaths.map((p) => p.trim()).filter((p) => p !== '')
			});
		} catch (e) {
			saveError = e instanceof Error ? e.message : String(e);
		} finally {
			saving = false;
		}
	}

	async function revert() {
		saving = true;
		saveError = '';
		try {
			await clearAgentConfig(host);
			agent = await getAgent(host);
			resetForm(agent);
		} catch (e) {
			saveError = e instanceof Error ? e.message : String(e);
		} finally {
			saving = false;
		}
	}

	// Two-step arm/confirm rather than a single click -- restart briefly
	// interrupts log collection for this one host, a higher blast
	// radius than the config edits above (which never take effect until
	// the agent's own next check-in, and never disrupt anything by
	// themselves). Resets if the user navigates the form instead of
	// confirming.
	let restartArmed = $state(false);
	let restarting = $state(false);
	let restartError = $state('');

	async function restart() {
		if (!restartArmed) {
			restartArmed = true;
			return;
		}
		restarting = true;
		restartError = '';
		try {
			agent = await issueAgentCommand(host, 'restart');
		} catch (e) {
			restartError = e instanceof Error ? e.message : String(e);
		} finally {
			restarting = false;
			restartArmed = false;
		}
	}

	function relativeTime(iso: string): string {
		const ms = Date.now() - new Date(iso).getTime();
		if (ms < 60_000) return `${Math.max(0, Math.round(ms / 1000))}s ago`;
		if (ms < 3_600_000) return `${Math.round(ms / 60_000)}m ago`;
		if (ms < 86_400_000) return `${Math.round(ms / 3_600_000)}h ago`;
		return `${Math.round(ms / 86_400_000)}d ago`;
	}
</script>

<main>
	<a class="back" href="/agents">← Agents</a>
	<h1>{host}</h1>

	{#if loading}
		<Skeleton height="12rem" />
	{:else if error}
		<p class="error">Error: {error}</p>
	{:else if agent}
		<section class="reported">
			<h2>Reported</h2>
			<dl>
				<dt>Service</dt>
				<dd>{agent.service}</dd>
				<dt>Version</dt>
				<dd>{agent.agent_version || '—'}</dd>
				<dt>Source</dt>
				<dd>{agent.source_kind}{agent.source_detail ? ` (${agent.source_detail})` : ''}</dd>
				<dt>First seen</dt>
				<dd>{relativeTime(agent.first_seen_at)}</dd>
				<dt>Last seen</dt>
				<dd>{relativeTime(agent.last_seen_at)}</dd>
			</dl>
		</section>

		<section class="config">
			<h2>
				Remote config
				{#if agent.pending}
					<Badge tone="accent">pending — agent hasn't applied this yet</Badge>
				{:else if agent.desired_override}
					<Badge tone="neutral">applied</Badge>
				{/if}
			</h2>
			<p class="hint">
				Changes here don't touch the agent's local config file -- they're an override the agent fetches and applies
				on its own schedule (its heartbeat interval), and revert automatically if the agent restarts before its next
				check-in re-syncs them. Connection details (TLS, ingest endpoint) are never remotely editable.
			</p>

			<div class="field">
				<label for="batch-max-size">Batch max size</label>
				<Input id="batch-max-size" type="number" min="1" bind:value={batchMaxSize} />
			</div>
			<div class="field">
				<label for="batch-flush-ms">Batch flush interval (ms)</label>
				<Input id="batch-flush-ms" type="number" min="100" bind:value={batchFlushIntervalMs} />
			</div>
			<div class="field checkbox">
				<label><input type="checkbox" bind:checked={heartbeatEnabled} /> Heartbeat enabled</label>
			</div>
			<div class="field">
				<label for="heartbeat-ms">Heartbeat interval (ms)</label>
				<Input id="heartbeat-ms" type="number" min="5000" bind:value={heartbeatIntervalMs} />
			</div>
			{#if agent.source_kind === 'journald'}
				<div class="field">
					<label for="journald-unit">Journald unit filter</label>
					<Input id="journald-unit" placeholder="(empty = whole journal)" bind:value={journaldUnit} />
				</div>
			{/if}
			<div class="field extra-paths">
				<span class="field-label">Additional log paths</span>
				<p class="hint">
					Extra files this agent should tail alongside its primary source above -- never a replacement for it. Applied
					the same way as every other field here, on the agent's next check-in.
				</p>
				{#each extraFilePaths as _, i}
					<div class="path-row">
						<Input placeholder="/var/log/example.log" bind:value={extraFilePaths[i]} />
						<Button variant="secondary" onclick={() => removeExtraFilePath(i)}>Remove</Button>
					</div>
				{/each}
				<Button variant="secondary" onclick={addExtraFilePath}>Add path</Button>
			</div>

			{#if saveError}<p class="error">Error: {saveError}</p>{/if}

			<div class="actions">
				<Button variant="primary" onclick={save} disabled={saving}>Save</Button>
				{#if agent.desired_override}
					<Button variant="secondary" onclick={revert} disabled={saving}>Revert to local config</Button>
				{/if}
			</div>
		</section>

		<section class="lifecycle">
			<h2>Lifecycle</h2>
			<p class="hint">
				Restart tells the agent to shut down gracefully (flushing anything buffered first) and exit -- it relies on the
				host's own service manager (systemd, Windows SCM) to bring it back up, same as this agent already expects from
				a normal crash or `systemctl restart`. Delivered on the agent's next check-in; there's no confirmation once
				it's been handed out, since a restarting agent can't report back before its process exits.
			</p>
			{#if agent.command_issued_at}
				<p class="hint">
					Last restart issued {relativeTime(agent.command_issued_at)}{agent.command_issued_by
						? ` by ${agent.command_issued_by}`
						: ''}.
				</p>
			{/if}
			{#if restartError}<p class="error">Error: {restartError}</p>{/if}
			<div class="actions">
				{#if restartArmed}
					<Button variant="danger" onclick={restart} disabled={restarting}>Confirm restart</Button>
					<Button variant="secondary" onclick={() => (restartArmed = false)} disabled={restarting}>Cancel</Button>
				{:else}
					<Button variant="secondary" onclick={restart}>Restart agent</Button>
				{/if}
			</div>
		</section>
	{/if}
</main>

<style>
	main {
		max-width: 40rem;
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
		margin: var(--space-2) 0 var(--space-5);
		font-family: var(--font-mono);
	}
	h2 {
		font-size: var(--text-base);
		display: flex;
		align-items: center;
		gap: var(--space-2);
		margin-bottom: var(--space-3);
	}
	.error {
		color: var(--color-danger);
	}
	.reported {
		margin-bottom: var(--space-6);
	}
	.lifecycle {
		margin-top: var(--space-6);
	}
	dl {
		display: grid;
		grid-template-columns: auto 1fr;
		gap: var(--space-1) var(--space-4);
		font-size: var(--text-sm);
	}
	dt {
		color: var(--color-text-muted);
	}
	dd {
		margin: 0;
	}
	.hint {
		color: var(--color-text-muted);
		font-size: var(--text-sm);
		margin-bottom: var(--space-4);
	}
	.field {
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
		margin-bottom: var(--space-3);
		max-width: 20rem;
	}
	.field.extra-paths {
		max-width: none;
	}
	.field label {
		font-size: var(--text-sm);
		color: var(--color-text-muted);
	}
	.field.checkbox label {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		color: var(--color-text);
	}
	.field-label {
		font-size: var(--text-sm);
		color: var(--color-text-muted);
	}
	.field .hint {
		margin-bottom: var(--space-2);
	}
	.path-row {
		display: flex;
		gap: var(--space-2);
		align-items: center;
		margin-bottom: var(--space-2);
	}
	.path-row :global(input) {
		flex: 1;
	}
	.actions {
		display: flex;
		gap: var(--space-3);
		margin-top: var(--space-5);
	}
</style>
