<script lang="ts">
	import QueryBar from '$lib/QueryBar.svelte';
	import {
		listNotificationTargets,
		createNotificationTarget,
		createRule,
		type NotificationTarget,
		type NotificationKind,
		type ConditionType,
		type Comparator,
		type Language
	} from '$lib/api';

	let name = $state('');
	let description = $state('');
	let query = $state('');
	let language = $state<Language>('');
	let conditionType = $state<ConditionType>('threshold');
	let comparator = $state<Comparator>('gt');
	let thresholdValue = $state('100');
	let evalIntervalSeconds = $state('60');
	let forMinutes = $state('0');
	let renotifyIntervalMinutes = $state('');

	let targets = $state<NotificationTarget[]>([]);
	let targetId = $state('');
	let showNewTarget = $state(false);
	let newTargetName = $state('');
	let newTargetKind = $state<NotificationKind>('webhook');
	let newTargetURL = $state('');

	let error = $state('');
	let submitting = $state(false);

	async function loadTargets() {
		try {
			targets = await listNotificationTargets();
			if (targets.length > 0 && !targetId) targetId = targets[0].id;
		} catch (e) {
			// Every other data-loading call on this page's siblings
			// (dashboards, alerts list, rule detail) wraps its fetch in
			// try/catch -- this one didn't, and an unhandled rejection here
			// (e.g. alerting unreachable) crashes the prerendering build
			// entirely rather than just showing an error, found by actually
			// running `docker build` for web.
			error = e instanceof Error ? e.message : String(e);
		}
	}
	loadTargets();

	async function submitNewTarget() {
		if (!newTargetName.trim() || !newTargetURL.trim()) return;
		try {
			const t = await createNotificationTarget({ name: newTargetName, kind: newTargetKind, webhook_url: newTargetURL });
			await loadTargets();
			targetId = t.id;
			showNewTarget = false;
			newTargetName = '';
			newTargetURL = '';
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		}
	}

	async function submit() {
		error = '';
		if (!name.trim() || !query.trim() || !targetId) {
			error = 'name, query, and a notification target are all required';
			return;
		}
		submitting = true;
		try {
			const payload: Record<string, unknown> = {
				name,
				description,
				query,
				query_language: language,
				condition_type: conditionType,
				eval_interval_seconds: Number(evalIntervalSeconds),
				for_minutes: Number(forMinutes),
				notification_target_id: targetId
			};
			if (conditionType === 'threshold') {
				payload.comparator = comparator;
				payload.threshold_value = Number(thresholdValue);
			}
			if (renotifyIntervalMinutes.trim() !== '') {
				payload.renotify_interval_minutes = Number(renotifyIntervalMinutes);
			}
			const rule = await createRule(payload);
			window.location.href = `/alerts/${rule.id}`;
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		} finally {
			submitting = false;
		}
	}
</script>

<main>
	<h1>New alert rule</h1>
	{#if error}<p class="error">Error: {error}</p>{/if}

	<label class="field">
		Name
		<input bind:value={name} placeholder="High error rate" />
	</label>
	<label class="field">
		Description
		<input bind:value={description} placeholder="optional" />
	</label>

	<QueryBar bind:query bind:language onRun={() => {}} placeholder="service=api | where status>=500 | stats count" />
	<p class="hint">
		For <code>threshold</code> rules, the query must resolve to exactly one row (e.g.
		<code>| stats count</code>). For <code>absence</code> rules, the query's own <code>earliest=</code>
		defines the window being checked for zero results.
	</p>

	<div class="row">
		<label>
			Condition
			<select bind:value={conditionType}>
				<option value="threshold">Threshold</option>
				<option value="absence">Absence</option>
			</select>
		</label>
		{#if conditionType === 'threshold'}
			<label>
				Comparator
				<select bind:value={comparator}>
					<option value="gt">&gt;</option>
					<option value="gte">&gt;=</option>
					<option value="lt">&lt;</option>
					<option value="lte">&lt;=</option>
					<option value="eq">==</option>
					<option value="ne">!=</option>
				</select>
			</label>
			<label>
				Threshold value
				<input type="number" bind:value={thresholdValue} />
			</label>
		{/if}
	</div>

	<div class="row">
		<label>
			Evaluation interval (seconds)
			<input type="number" min="30" bind:value={evalIntervalSeconds} />
		</label>
		<label>
			Debounce, "for" minutes
			<input type="number" min="0" bind:value={forMinutes} />
		</label>
		<label>
			Renotify interval (minutes, optional)
			<input type="number" min="1" bind:value={renotifyIntervalMinutes} placeholder="never" />
		</label>
	</div>

	<div class="row">
		<label>
			Notification target
			<select bind:value={targetId}>
				{#each targets as t (t.id)}
					<option value={t.id}>{t.name} ({t.kind})</option>
				{/each}
			</select>
		</label>
		<button type="button" onclick={() => (showNewTarget = !showNewTarget)}>
			{showNewTarget ? 'Cancel' : '+ New target'}
		</button>
	</div>

	{#if showNewTarget}
		<div class="new-target">
			<input placeholder="Target name" bind:value={newTargetName} />
			<select bind:value={newTargetKind}>
				<option value="webhook">Generic webhook</option>
				<option value="slack">Slack</option>
				<option value="pagerduty">PagerDuty</option>
			</select>
			<input placeholder="https://..." bind:value={newTargetURL} />
			<button type="button" onclick={submitNewTarget}>Add target</button>
		</div>
	{/if}

	<button class="submit" onclick={submit} disabled={submitting}>
		{submitting ? 'Creating…' : 'Create rule'}
	</button>
</main>

<style>
	main {
		max-width: 45rem;
	}
	.error {
		color: var(--color-danger);
	}
	.field {
		display: block;
		margin-bottom: var(--space-3);
		font-size: var(--text-sm);
		color: var(--color-text-muted);
	}
	.field input {
		display: block;
		width: 100%;
		box-sizing: border-box;
		margin-top: var(--space-1);
	}
	.hint {
		font-size: var(--text-sm);
		color: var(--color-text-muted);
	}
	.row {
		display: flex;
		gap: var(--space-4);
		align-items: flex-end;
		margin: var(--space-4) 0;
		flex-wrap: wrap;
	}
	.row label {
		font-size: var(--text-sm);
		color: var(--color-text-muted);
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
	}
	.new-target {
		display: flex;
		gap: var(--space-2);
		margin-bottom: var(--space-4);
	}
	.submit {
		margin-top: var(--space-4);
		padding: var(--space-2) var(--space-4);
	}
</style>
