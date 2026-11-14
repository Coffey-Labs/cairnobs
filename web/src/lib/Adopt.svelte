<script lang="ts">
	// What should run on one host: the containers that must stay running,
	// the ones stopped on purpose, and the systemd services that must stay
	// active. "Adopt what's running" starts from what the agent reports now.
	import Icon from '$lib/Icon.svelte';
	import Panel from '$lib/Panel.svelte';
	import { get, send, type ExpectedSettings } from '$lib/api';
	import { live } from '$lib/live.svelte';

	let { host, onchange }: { host: string; onchange: () => void } = $props();

	type Choice = 'run' | 'stopped' | 'ignore';
	type Row = { name: string; choice: Choice; note: string; listed: boolean };
	type Svc = { name: string; on: boolean; listed: boolean };

	let data = $state<ExpectedSettings | null>(null);
	let error = $state('');
	let editing = $state(false);
	let rows = $state<Row[]>([]);
	let svcs = $state<Svc[]>([]);
	let addName = $state('');
	let addSvc = $state('');
	let svcFilter = $state('');
	let busy = $state(false);
	let saveError = $state('');

	async function load() {
		try {
			data = await get<ExpectedSettings>(`/api/v1/settings/expected/hosts/${encodeURIComponent(host)}`);
			error = '';
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		}
	}
	$effect(() => {
		void host;
		editing = false;
		void load();
	});

	// path.Match's syntax, near enough: * any run, ? one character, [..] a class.
	function glob(p: string): RegExp {
		let re = '';
		for (let i = 0; i < p.length; i++) {
			const c = p[i];
			if (c === '*') re += '.*';
			else if (c === '?') re += '.';
			else if (c === '[') {
				const j = p.indexOf(']', i + 1);
				if (j < 0) re += '\\[';
				else {
					re += '[' + p.slice(i + 1, j).replace(/^\^/, '^').replace(/\\/g, '\\\\') + ']';
					i = j;
				}
			} else re += c.replace(/[.+^${}()|\\]/g, '\\$&');
		}
		return new RegExp('^' + re + '$');
	}
	const isPattern = (p: string) => /[*?[]/.test(p);
	const unit = (n: string) => (n.includes('.') ? n : n + '.service');
	const matched = (p: string) => (data?.containers ?? []).filter((c) => glob(p).test(c.name));

	const expected = $derived(data?.expected);
	const count = $derived(expected ? expected.running.length + expected.stopped.length + expected.units.length : 0);

	function start() {
		if (!data) return;
		const e = data.expected;
		const out: Row[] = [
			...e.running.map((p) => ({ name: p, choice: 'run' as Choice, note: '', listed: true })),
			...e.stopped.map((s) => ({ name: s.match, choice: 'stopped' as Choice, note: s.note, listed: true }))
		];
		const pats = out.map((r) => glob(r.name));
		for (const c of data.containers ?? []) {
			if (!pats.some((re) => re.test(c.name))) out.push({ name: c.name, choice: c.state === 'running' ? 'run' : 'ignore', note: '', listed: false });
		}
		rows = out;
		const running = new Set(data.services ?? []);
		const s: Svc[] = e.units.map((u) => ({ name: u, on: true, listed: true }));
		for (const n of running) if (!s.some((x) => unit(x.name) === n)) s.push({ name: n, on: false, listed: false });
		svcs = s;
		addName = addSvc = svcFilter = saveError = '';
		editing = true;
	}
	function addRow() {
		const n = addName.trim();
		if (n && !rows.some((r) => r.name === n)) rows = [...rows, { name: n, choice: 'run', note: '', listed: false }];
		addName = '';
	}
	function addService() {
		const n = addSvc.trim();
		if (n && !svcs.some((s) => unit(s.name) === unit(n))) svcs = [{ name: n, on: true, listed: false }, ...svcs];
		addSvc = '';
	}
	async function save() {
		busy = true;
		saveError = '';
		try {
			await send(`/api/v1/settings/expected/hosts/${encodeURIComponent(host)}`, 'PUT', {
				running: rows.filter((r) => r.choice === 'run').map((r) => r.name),
				stopped: rows.filter((r) => r.choice === 'stopped').map((r) => ({ match: r.name, note: r.note })),
				units: svcs.filter((s) => s.on).map((s) => s.name)
			});
			editing = false;
			await load();
			onchange();
		} catch (e) {
			saveError = e instanceof Error ? e.message : String(e);
		} finally {
			busy = false;
		}
	}
	const shownSvcs = $derived(svcs.filter((s) => !svcFilter || s.name.toLowerCase().includes(svcFilter.toLowerCase())));
	const nowWord = (r: Row) => {
		if (isPattern(r.name)) {
			const m = matched(r.name);
			return m.length ? `matches ${m.map((c) => c.name).join(', ')}` : 'matches nothing now';
		}
		const c = data?.containers?.find((x) => x.name === r.name);
		return c ? c.state : 'not on this host now';
	};
</script>

{#if data && (data.source || count)}
	<Panel title="What should run here" aside={data.source === 'file' ? `set in ${data.file}` : 'checked against every report'}>
		{#if !editing}
			<p class="sum">
				{#if count}
					{expected?.running.length} {expected?.running.length === 1 ? 'container' : 'containers'} must stay running,
					{expected?.stopped.length} stopped on purpose,
					{expected?.units.length} {expected?.units.length === 1 ? 'service' : 'services'} must stay active.
					Anything else running here is flagged as not expected.
				{:else}
					Nothing is expected here yet, so containers are judged on Docker's state alone and no services are watched. Adopt what's running now, and anything that stops or goes missing later alerts.
				{/if}
			</p>
			{#if data.source === 'file'}
				<p class="sub">This is set in <span class="mono">{data.file}</span>, so it's changed there.</p>
			{:else if data.source === 'ui' && live.admin}
				<div class="row">
					<button class="btn sm pri" onclick={start} disabled={!data.reported && !count}>
						<Icon name={count ? 'pencil' : 'plus'} />{count ? 'Change' : 'Adopt what’s running'}
					</button>
					{#if !data.reported && !count}<span class="sub">Waiting for this host's first report.</span>{/if}
				</div>
			{/if}
		{:else}
			<h3>Containers</h3>
			{#if data.containers === null}<p class="sub">This host's agent doesn't report containers{data.containers_error ? `: ${data.containers_error}` : ' (no Docker helper)'}. Names can still be added below.</p>{/if}
			<div class="tbl-wrap flat">
				<table class="tbl">
					<thead><tr><th>Container</th><th>Now</th><th>Should be</th></tr></thead>
					<tbody>
						{#each rows as r, i (r.name)}
							<tr class:off={r.choice === 'ignore'}>
								<td class="mono">{r.name}{#if !r.listed && r.choice !== 'ignore'}{' '}<span class="new">new</span>{/if}</td>
								<td class="sub">{nowWord(r)}</td>
								<td>
									<select bind:value={rows[i].choice} aria-label="What {r.name} should be">
										<option value="run">Running</option>
										<option value="stopped">Stopped on purpose</option>
										<option value="ignore">Not watched</option>
									</select>
									{#if r.choice === 'stopped'}<input class="note" bind:value={rows[i].note} placeholder="Why (shown on the page)" aria-label="Why {r.name} is stopped" />{/if}
								</td>
							</tr>
						{:else}
							<tr><td colspan="3" class="empty">No containers on this host.</td></tr>
						{/each}
					</tbody>
				</table>
			</div>
			<form class="add" onsubmit={(e) => { e.preventDefault(); addRow(); }}>
				<input bind:value={addName} placeholder="Another name, or a pattern like web-*" aria-label="Add a container name or pattern" />
				<button class="btn sm" type="submit" disabled={!addName.trim()}>Add</button>
			</form>

			<h3>System services</h3>
			{#if data.services === null}
				<p class="sub">This host's agent doesn't list its services: it's older than this server, or the host has no systemd. Services can still be named below.</p>
			{:else}
				<p class="sub">Tick the ones that must stay active. One that stops, or isn't installed, is critical.</p>
			{/if}
			<form class="add" onsubmit={(e) => { e.preventDefault(); addService(); }}>
				{#if svcs.length > 12}<input bind:value={svcFilter} placeholder="Filter services" aria-label="Filter services" />{/if}
				<input bind:value={addSvc} placeholder="Another service, e.g. nginx" aria-label="Add a service" />
				<button class="btn sm" type="submit" disabled={!addSvc.trim()}>Add</button>
			</form>
			<div class="svcs">
				{#each shownSvcs as s (s.name)}
					<label class="tick"><input type="checkbox" bind:checked={s.on} /> <span class="mono">{s.name}</span>{#if data.services && !data.services.includes(unit(s.name))}{' '}<span class="gone">not running now</span>{/if}</label>
				{:else}
					<p class="sub">{svcFilter ? 'None match.' : 'No services.'}</p>
				{/each}
			</div>

			<div class="row">
				<button class="btn pri" onclick={save} disabled={busy}>Save</button>
				<button class="btn" onclick={() => (editing = false)}>Cancel</button>
				{#if saveError}<span class="err" role="alert">{saveError}</span>{/if}
			</div>
		{/if}
		{#if error}<p class="err">{error}</p>{/if}
	</Panel>
{:else if data}
	<Panel title="What should run here">
		<p class="sum">Cairn OBS has no database configured, so this can't be set here. Set <span class="mono">database</span> in obs-beacond.yaml, or point <span class="mono">expected</span> at an expected.yaml (docs/formats.md).</p>
	</Panel>
{:else if error}
	<p class="err">{error}</p>
{/if}

<style>
	.sum { margin: 0 0 12px; font-size: 13.5px; }
	.sm { height: 30px; padding: 0 12px; font-size: 13px; }
	h3 { font-size: 13px; margin: 6px 0 8px; color: var(--muted-fg); font-weight: 600; letter-spacing: .02em; }
	h3 + .sub, .sub + .tbl-wrap { margin-top: 0; }
	.flat { border: 1px solid var(--border); border-radius: var(--radius); }
	tr.off td { opacity: .6; }
	select, input:not([type=checkbox]) { height: 32px; padding: 0 8px; border-radius: var(--radius); border: 1px solid var(--input); background: var(--background); color: var(--foreground); font: inherit; font-size: 13px; }
	.note { margin-left: 8px; width: 220px; max-width: 100%; }
	.new { font-size: 11px; color: var(--primary); font-family: var(--font-sans, inherit); }
	.add { display: flex; gap: 8px; flex-wrap: wrap; margin: 10px 0 18px; }
	.add input { flex: 1 1 200px; min-width: 0; }
	.svcs { display: grid; grid-template-columns: repeat(auto-fill, minmax(240px, 1fr)); gap: 2px 18px; margin-bottom: 18px; max-height: 360px; overflow: auto; }
	.tick { display: flex; align-items: center; gap: 8px; font-size: 13px; min-height: 26px; min-width: 0; }
	.tick .mono { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
	.gone { color: var(--warning); font-size: 12px; white-space: nowrap; }
	.row { display: flex; gap: 8px; flex-wrap: wrap; align-items: center; }
	.err { color: var(--critical); font-size: 13px; margin: 0; }
	@media (max-width: 600px) { .note { margin: 6px 0 0; width: 100%; } }
</style>
