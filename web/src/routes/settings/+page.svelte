<script lang="ts">
	import {
		getAuthFeatures,
		enterpriseAuthBase,
		localAuthEnabled,
		getLocalSession,
		getCurrentSession,
		listRetentionHosts,
		previewLogDeletion,
		deleteLogsOlderThan,
		type AuthFeatures,
		type LocalSession,
		type CurrentSession,
		type RetentionHost,
		type HostService,
		type LogRetentionPreview,
		type LogRetentionDeleteResult
	} from '$lib/api';
	import { getTheme, setTheme, type Theme } from '$lib/theme.svelte';
	import { getDensity, setDensity, type Density } from '$lib/density.svelte';

	let loading = $state(true);
	let features = $state<AuthFeatures>({ sso_configured: false, oidc_enabled: false, saml_enabled: false });

	async function load() {
		loading = true;
		features = await getAuthFeatures();
		loading = false;
	}
	load();

	// --- log retention gating (owner/admin only, see api/logretention) ---
	let localSession = $state<LocalSession | 'disabled' | null>(null);
	let currentSession = $state<CurrentSession | null>(null);
	$effect(() => {
		if (localAuthEnabled) getLocalSession().then((s) => (localSession = s));
	});
	$effect(() => {
		if (enterpriseAuthBase) getCurrentSession().then((s) => (currentSession = s));
	});

	// A deployment with neither enterprise SSO nor local auth configured
	// has no session concept at all -- same "Phase 0-3 default-open"
	// posture RequireRole's nil-authorizer no-op gives the server side
	// (see api/logretention/handler.go), so nothing is hidden here
	// either.
	//
	// localAuthEnabled is checked first, not enterpriseAuthBase --
	// enterpriseAuthBase only means enterprise-auth is *deployed and
	// reachable* (e.g. for a "switch tenant" link), not that SSO is
	// what's actually authenticating this browser session. main.go picks
	// ENTERPRISE_AUTH_URL over LOCAL_AUTH_ENABLED for which *server-side*
	// authorizer is live, but that's an independent, server-only choice
	// -- a deployment can (and this repo's own production deployment
	// does) run enterprise-auth alongside a local-auth-only api, so a
	// real signed-in local session must win here even though
	// enterpriseAuthBase is also set. Falls through to the enterprise
	// session only if local auth isn't what actually resolved a session
	// for this browser.
	const canManageRetention = $derived.by(() => {
		if (localAuthEnabled) {
			const s = localSession;
			if (s !== null && s !== 'disabled') {
				return s.role === 'owner' || s.role === 'admin';
			}
		}
		if (enterpriseAuthBase) {
			const s = currentSession;
			return s !== null && (s.role === 'owner' || s.role === 'admin');
		}
		return !localAuthEnabled;
	});

	// --- log retention deletion -- scoped to specific (host, service)
	// targets, not wholesale: a caller picks which agents' *and* which log
	// types' logs to target from what listRetentionHosts reports for the
	// chosen age, same "select specific targets, not delete everything"
	// requirement api/logretention's own parseTargets enforces
	// server-side. ---
	const retentionOptions: { label: string; hours: number }[] = [
		{ label: '7 days', hours: 24 * 7 },
		{ label: '30 days', hours: 24 * 30 },
		{ label: '90 days', hours: 24 * 90 },
		{ label: '180 days', hours: 24 * 180 },
		{ label: '365 days', hours: 24 * 365 }
	];
	let retentionHours = $state(retentionOptions[1].hours);

	let hostsLoading = $state(false);
	let hosts = $state<RetentionHost[]>([]);
	let hostsError = $state('');
	// Selection keyed by a composite "host service" string for O(1)
	// membership checks -- targetKey/targetsFromKeys convert to/from the
	// {host, service} objects the API actually wants.
	let selectedTargets = $state<Set<string>>(new Set());

	let previewing = $state(false);
	let preview = $state<LogRetentionPreview | null>(null);
	let deleting = $state(false);
	let deleteResult = $state<LogRetentionDeleteResult | null>(null);
	let retentionError = $state('');

	function targetKey(host: string, service: string): string {
		return `${host} ${service}`;
	}

	function targetsFromKeys(keys: Iterable<string>): HostService[] {
		return [...keys].map((key) => {
			const [host, service] = key.split(' ');
			return { host, service };
		});
	}

	// Reloads whenever retentionHours changes (including on mount, since
	// $effect runs once immediately too) -- selection/preview/result all
	// reset because they were scoped to the previous age's host list.
	async function loadHosts(hours: number) {
		hostsLoading = true;
		hostsError = '';
		preview = null;
		deleteResult = null;
		selectedTargets = new Set();
		try {
			const result = await listRetentionHosts(hours);
			hosts = result.hosts;
		} catch (e) {
			hostsError = e instanceof Error ? e.message : String(e);
			hosts = [];
		} finally {
			hostsLoading = false;
		}
	}
	$effect(() => {
		loadHosts(retentionHours);
	});

	function toggleTarget(host: string, service: string) {
		const key = targetKey(host, service);
		const next = new Set(selectedTargets);
		if (next.has(key)) next.delete(key);
		else next.add(key);
		selectedTargets = next;
		preview = null;
		deleteResult = null;
	}

	function isHostFullySelected(host: RetentionHost): boolean {
		return host.services.length > 0 && host.services.every((s) => selectedTargets.has(targetKey(host.host, s.service)));
	}

	// Toggles every service under one host together -- selects all of
	// them if any are currently unselected, otherwise clears all of them.
	function toggleHostAll(host: RetentionHost) {
		const next = new Set(selectedTargets);
		const selectAll = !isHostFullySelected(host);
		for (const s of host.services) {
			const key = targetKey(host.host, s.service);
			if (selectAll) next.add(key);
			else next.delete(key);
		}
		selectedTargets = next;
		preview = null;
		deleteResult = null;
	}

	function selectAllTargets() {
		const next = new Set<string>();
		for (const h of hosts) {
			for (const s of h.services) next.add(targetKey(h.host, s.service));
		}
		selectedTargets = next;
		preview = null;
	}

	function selectNoTargets() {
		selectedTargets = new Set();
		preview = null;
	}

	async function handlePreview() {
		if (selectedTargets.size === 0) return;
		previewing = true;
		retentionError = '';
		deleteResult = null;
		try {
			preview = await previewLogDeletion(retentionHours, targetsFromKeys(selectedTargets));
		} catch (e) {
			retentionError = e instanceof Error ? e.message : String(e);
		} finally {
			previewing = false;
		}
	}

	function cancelPreview() {
		preview = null;
	}

	async function confirmDelete() {
		if (!preview) return;
		deleting = true;
		retentionError = '';
		try {
			// preview.targets, not the current selection -- already excludes
			// any target the preview found protected, so this never re-asks
			// for a target the response just said would be skipped.
			const result = await deleteLogsOlderThan(retentionHours, preview.targets);
			preview = null;
			// loadHosts resets deleteResult as part of its "fresh state" load
			// (stale counts/now-empty hosts shouldn't linger), so it runs
			// before deleteResult is set here, not after.
			await loadHosts(retentionHours);
			deleteResult = result;
		} catch (e) {
			retentionError = e instanceof Error ? e.message : String(e);
		} finally {
			deleting = false;
		}
	}

	function formatTarget(t: HostService): string {
		return `${t.host}/${t.service}`;
	}

	function formatCutoff(iso: string): string {
		return new Date(iso).toLocaleString(undefined, {
			year: 'numeric',
			month: 'short',
			day: 'numeric',
			hour: 'numeric',
			minute: '2-digit'
		});
	}

	const themeOptions: { value: Theme; label: string; hint: string }[] = [
		{ value: 'dark', label: 'Dark', hint: 'Default' },
		{ value: 'light', label: 'Light', hint: '' },
		{ value: 'system', label: 'System', hint: 'Follow OS setting' }
	];
	const densityOptions: { value: Density; label: string; hint: string }[] = [
		{ value: 'comfortable', label: 'Comfortable', hint: 'Dashboards, forms' },
		{ value: 'compact', label: 'Compact', hint: 'Log tables, results' }
	];
</script>

<main>
	<h1>Settings</h1>

	<section>
		<h2>Appearance</h2>
		<div class="option-group" role="radiogroup" aria-label="Theme">
			{#each themeOptions as opt (opt.value)}
				<button
					type="button"
					class="option"
					class:selected={getTheme() === opt.value}
					aria-pressed={getTheme() === opt.value}
					onclick={() => setTheme(opt.value)}
				>
					<span class="option-label">{opt.label}</span>
					{#if opt.hint}<span class="option-hint">{opt.hint}</span>{/if}
				</button>
			{/each}
		</div>
		<div class="option-group" role="radiogroup" aria-label="Row density">
			{#each densityOptions as opt (opt.value)}
				<button
					type="button"
					class="option"
					class:selected={getDensity() === opt.value}
					aria-pressed={getDensity() === opt.value}
					onclick={() => setDensity(opt.value)}
				>
					<span class="option-label">{opt.label}</span>
					<span class="option-hint">{opt.hint}</span>
				</button>
			{/each}
		</div>
	</section>

	<section>
		<h2>Core</h2>
		<p>Single-tenant deployment settings live here. Nothing configurable yet.</p>
		{#if localAuthEnabled}
			<p class="note">Manage accounts, passwords, and roles from <a href="/users">Users</a>.</p>
		{/if}
	</section>

	{#if canManageRetention}
		<section>
			<h2>Log retention</h2>
			<p class="note">
				Permanently delete specific log types from specific hosts, older than a chosen age. Visible to
				owners and admins only.
			</p>

			<div class="retention-controls">
				<select bind:value={retentionHours} disabled={hostsLoading || previewing || deleting}>
					{#each retentionOptions as opt (opt.hours)}
						<option value={opt.hours}>Older than {opt.label}</option>
					{/each}
				</select>
			</div>

			{#if hostsError}<p class="error">{hostsError}</p>{/if}

			{#if hostsLoading}
				<p class="muted">Loading hosts…</p>
			{:else if hosts.length === 0}
				<p class="note">No hosts have logs older than this.</p>
			{:else}
				<div class="host-picker">
					<div class="host-picker-actions">
						<button type="button" class="link" onclick={selectAllTargets} disabled={deleting}>Select all</button>
						<button type="button" class="link" onclick={selectNoTargets} disabled={deleting}>Select none</button>
					</div>
					<ul class="host-list">
						{#each hosts as h (h.host)}
							<li class="host-group">
								<label class="host-row">
									<input
										type="checkbox"
										checked={isHostFullySelected(h)}
										disabled={deleting}
										onchange={() => toggleHostAll(h)}
									/>
									<span class="host-name">{h.host}</span>
									{#if h.protected_days != null}
										<span class="protected-badge">host default {h.protected_days}d</span>
									{/if}
								</label>
								<ul class="service-list">
									{#each h.services as s (s.service)}
										<li>
											<label>
												<input
													type="checkbox"
													checked={selectedTargets.has(targetKey(h.host, s.service))}
													disabled={deleting}
													onchange={() => toggleTarget(h.host, s.service)}
												/>
												<span class="service-name">{s.service}</span>
												<span class="host-count">{s.count.toLocaleString()} records</span>
												{#if s.protected_days != null}
													<span class="protected-badge">protected {s.protected_days}d</span>
												{/if}
											</label>
										</li>
									{/each}
								</ul>
							</li>
						{/each}
					</ul>
					<button
						type="button"
						onclick={handlePreview}
						disabled={selectedTargets.size === 0 || previewing || deleting}
					>
						{previewing
							? 'Checking…'
							: `Delete logs from ${selectedTargets.size} target${selectedTargets.size === 1 ? '' : 's'}…`}
					</button>
				</div>
			{/if}

			{#if retentionError}<p class="error">{retentionError}</p>{/if}

			{#if preview}
				<div class="confirm-panel">
					{#if preview.targets.length > 0}
						<p>
							This will <strong>permanently delete {preview.count.toLocaleString()}</strong>
							log record{preview.count === 1 ? '' : 's'} older than {formatCutoff(preview.cutoff)} from
							{preview.targets.length} target{preview.targets.length === 1 ? '' : 's'} ({preview.targets
								.map(formatTarget)
								.join(', ')}). This cannot be undone.
						</p>
					{:else}
						<p>Every selected target is protected by a retention policy -- nothing to delete.</p>
					{/if}
					{#if preview.blocked_targets?.length}
						<p class="note">
							Protected by a retention policy, skipped: {preview.blocked_targets
								.map((b) => `${formatTarget(b)} (${b.protected_days}d)`)
								.join(', ')}.
						</p>
					{/if}
					<div class="confirm-actions">
						<button type="button" onclick={cancelPreview} disabled={deleting}>
							{preview.targets.length > 0 ? 'Cancel' : 'Close'}
						</button>
						{#if preview.targets.length > 0}
							<button type="button" class="danger" onclick={confirmDelete} disabled={deleting}>
								{deleting ? 'Deleting…' : 'Yes, delete permanently'}
							</button>
						{/if}
					</div>
				</div>
			{/if}

			{#if deleteResult}
				<p class="note">
					Deleted {deleteResult.deleted_count.toLocaleString()} log record{deleteResult.deleted_count === 1
						? ''
						: 's'} older than {formatCutoff(deleteResult.cutoff)}
					{#if deleteResult.deleted_targets.length}from {deleteResult.deleted_targets
							.map(formatTarget)
							.join(', ')}{/if}.
					{#if deleteResult.blocked_targets?.length}
						Skipped (protected): {deleteResult.blocked_targets
							.map((b) => `${formatTarget(b)} (${b.protected_days}d)`)
							.join(', ')}.
					{/if}
				</p>
			{/if}
		</section>
	{/if}

	{#if loading}
		<p class="muted">Loading…</p>
	{:else if features.sso_configured}
		<section>
			<h2>Single sign-on</h2>
			<ul>
				{#if features.oidc_enabled}<li>OIDC configured</li>{/if}
				{#if features.saml_enabled}<li>SAML configured</li>{/if}
			</ul>
			<p class="note">
				User/role management isn't built yet -- an Admin or Owner will manage tenant members here once
				it is.
			</p>
		</section>
	{:else if enterpriseAuthBase}
		<section>
			<h2>Single sign-on</h2>
			<p class="note">enterprise-auth is deployed but no SSO provider is configured yet.</p>
		</section>
	{/if}
	<!-- No section at all when enterpriseAuthBase is unset -- a Phase 0-3-style
	     single-tenant deployment with no enterprise/ deployed sees a settings
	     page with core content only, no dead "upgrade to unlock" link. See
	     /docs/phase-4-rbac-design.md's "Web UI boundary" section. -->
</main>

<style>
	main {
		max-width: 40rem;
	}
	h1 {
		font-size: var(--text-xl);
		margin-bottom: var(--space-5);
	}
	section {
		margin-bottom: var(--space-6);
	}
	h2 {
		font-size: var(--text-md);
		margin-bottom: var(--space-3);
	}
	.muted {
		color: var(--color-text-muted);
	}
	.note {
		color: var(--color-text-muted);
		font-size: var(--text-sm);
	}
	.note a {
		color: var(--color-accent);
	}
	.option-group {
		display: flex;
		gap: var(--space-2);
		margin-bottom: var(--space-3);
	}
	.option {
		flex: 1;
		display: flex;
		flex-direction: column;
		align-items: flex-start;
		gap: var(--space-1);
		padding: var(--space-3);
		border: 1px solid var(--color-border);
		border-radius: var(--radius-md);
		background: var(--color-surface);
		cursor: pointer;
		text-align: left;
	}
	.option:hover {
		border-color: var(--color-border-strong);
	}
	.option.selected {
		border-color: var(--color-accent);
		background: color-mix(in srgb, var(--color-accent) 8%, var(--color-surface));
	}
	.option-label {
		font-weight: var(--font-weight-medium);
		color: var(--color-text);
	}
	.option-hint {
		font-size: var(--text-xs);
		color: var(--color-text-muted);
	}
	/* The accent-tinted .selected background is *lighter* than plain
	   --color-surface, which quietly drops --color-text-muted below
	   4.5:1 (axe-core's color-contrast check caught this at 4.4:1) --
	   fixed by using the full-strength --color-text on that lighter
	   background specifically, not by changing the shared muted token
	   everywhere else it still passes correctly. */
	.option.selected .option-hint {
		color: var(--color-text);
	}

	.error {
		color: var(--color-danger);
		font-size: var(--text-sm);
		margin-top: var(--space-2);
	}
	.retention-controls {
		display: flex;
		gap: var(--space-2);
		align-items: center;
		margin-top: var(--space-3);
	}
	.retention-controls select {
		font-family: var(--font-ui);
		font-size: var(--text-sm);
		color: var(--color-text);
		background: var(--color-surface);
		border: 1px solid var(--color-border);
		border-radius: var(--radius-sm);
		padding: var(--space-2) var(--space-3);
	}
	.host-picker {
		margin-top: var(--space-3);
		padding: var(--space-3);
		background: var(--color-surface);
		border: 1px solid var(--color-border);
		border-radius: var(--radius-md);
	}
	.host-picker-actions {
		display: flex;
		gap: var(--space-3);
		margin-bottom: var(--space-2);
	}
	.link {
		background: none;
		border: none;
		padding: 0;
		color: var(--color-accent);
		font-family: var(--font-ui);
		font-size: var(--text-xs);
		cursor: pointer;
	}
	.link:disabled {
		cursor: default;
		opacity: 0.6;
	}
	.host-list {
		list-style: none;
		margin: 0 0 var(--space-3);
		padding: 0;
		max-height: 20rem;
		overflow-y: auto;
	}
	.host-group {
		border-bottom: 1px solid var(--color-border);
	}
	.host-group:last-child {
		border-bottom: none;
	}
	.host-row {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		padding: var(--space-2) var(--space-1);
		font-size: var(--text-sm);
		font-weight: var(--font-weight-medium);
		cursor: pointer;
	}
	.service-list {
		list-style: none;
		margin: 0;
		padding: 0 0 var(--space-2);
	}
	.service-list label {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		padding: var(--space-1) var(--space-1) var(--space-1) var(--space-5);
		font-size: var(--text-sm);
		cursor: pointer;
	}
	.host-name {
		font-family: var(--font-mono);
		color: var(--color-text);
	}
	.service-name {
		font-family: var(--font-mono);
		color: var(--color-text-muted);
	}
	.host-count {
		color: var(--color-text-muted);
		font-size: var(--text-xs);
	}
	.protected-badge {
		margin-left: auto;
		font-size: var(--text-xs);
		color: var(--color-text-muted);
		border: 1px solid var(--color-border);
		border-radius: var(--radius-sm);
		padding: 0.05rem 0.4rem;
	}
	.host-picker > button {
		padding: var(--space-2) var(--space-4);
		font-family: var(--font-ui);
		font-size: var(--text-sm);
		font-weight: var(--font-weight-medium);
		color: var(--color-text);
		background: var(--color-bg);
		border: 1px solid var(--color-border);
		border-radius: var(--radius-sm);
		cursor: pointer;
	}
	.host-picker > button:hover:not(:disabled) {
		border-color: var(--color-border-strong);
	}
	.host-picker > button:disabled {
		cursor: default;
		opacity: 0.6;
	}
	.confirm-panel {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
		margin-top: var(--space-3);
		padding: var(--space-4);
		background: var(--color-surface);
		border: 1px solid var(--color-danger);
		border-radius: var(--radius-md);
	}
	.confirm-panel p {
		font-size: var(--text-sm);
		color: var(--color-text);
	}
	.confirm-actions {
		display: flex;
		gap: var(--space-2);
	}
	.confirm-actions button {
		padding: var(--space-2) var(--space-4);
		font-family: var(--font-ui);
		font-size: var(--text-sm);
		font-weight: var(--font-weight-medium);
		border-radius: var(--radius-sm);
		cursor: pointer;
	}
	.confirm-actions button:not(.danger) {
		color: var(--color-text);
		background: var(--color-surface);
		border: 1px solid var(--color-border);
	}
	.confirm-actions button.danger {
		color: var(--color-bg);
		background: var(--color-danger);
		border: none;
	}
	.confirm-actions button:disabled {
		cursor: default;
		opacity: 0.6;
	}
</style>
