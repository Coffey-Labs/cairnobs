<script lang="ts">
	import { page } from '$app/state';
	import { getCurrentSession, enterpriseAuthBase, type CurrentSession } from '$lib/api';
	import { getTheme, setTheme, type Theme } from '$lib/theme.svelte';
	import { getDensity, toggleDensity } from '$lib/density.svelte';

	let {
		onOpenPalette,
		mobileOpen = false,
		onCloseMobile
	}: { onOpenPalette: () => void; mobileOpen?: boolean; onCloseMobile?: () => void } = $props();

	const navItems = [
		{ href: '/', label: 'Search', icon: '◇' },
		{ href: '/dashboards', label: 'Dashboards', icon: '▤' },
		{ href: '/alerts', label: 'Alerts', icon: '▲' },
		{ href: '/data-sources', label: 'Data Sources', icon: '◈' },
		{ href: '/agents', label: 'Agents', icon: '●' },
		{ href: '/settings', label: 'Settings', icon: '⚙' }
	];

	function isActive(href: string): boolean {
		if (href === '/') return page.url.pathname === '/';
		return page.url.pathname.startsWith(href);
	}

	let session: CurrentSession | null = $state(null);
	$effect(() => {
		getCurrentSession().then((s) => (session = s));
	});

	const themeOptions: { value: Theme; label: string }[] = [
		{ value: 'dark', label: 'Dark' },
		{ value: 'light', label: 'Light' },
		{ value: 'system', label: 'System' }
	];
</script>

{#if mobileOpen}
	<button type="button" class="backdrop" onclick={onCloseMobile} aria-label="Close menu"></button>
{/if}

<aside class="sidebar" class:mobile-open={mobileOpen}>
	<div class="brand">
		<span class="mark" aria-hidden="true">◆</span>
		<span class="name">Sentry</span>
		<button type="button" class="close-mobile" onclick={onCloseMobile} aria-label="Close menu">✕</button>
	</div>

	{#if enterpriseAuthBase}
		<div class="tenant">
			{#if session}
				<div class="tenant-pill">
					<span class="dot" aria-hidden="true"></span>
					<span class="tenant-name">{session.tenant_id}</span>
					<span class="role">{session.role}</span>
				</div>
				<a class="switch" href="{enterpriseAuthBase}/auth/oidc/login">Switch tenant</a>
			{:else}
				<a class="switch signin" href="{enterpriseAuthBase}/auth/oidc/login">Sign in</a>
			{/if}
		</div>
	{/if}

	<nav aria-label="Main">
		{#each navItems as item (item.href)}
			<a
				href={item.href}
				class:active={isActive(item.href)}
				aria-current={isActive(item.href) ? 'page' : undefined}
				onclick={onCloseMobile}
			>
				<span class="ic" aria-hidden="true">{item.icon}</span>
				{item.label}
			</a>
		{/each}
	</nav>

	<div class="footer">
		<button type="button" class="palette-hint" onclick={onOpenPalette}>
			<span>Jump to…</span>
			<kbd>⌘K</kbd>
		</button>

		<div class="controls">
			<label for="theme-select" class="sr-only">Theme</label>
			<select id="theme-select" value={getTheme()} onchange={(e) => setTheme(e.currentTarget.value as Theme)}>
				{#each themeOptions as opt (opt.value)}
					<option value={opt.value}>{opt.label}</option>
				{/each}
			</select>
			<button type="button" class="density-toggle" onclick={toggleDensity} title="Toggle row density">
				{getDensity() === 'compact' ? 'Compact' : 'Comfortable'}
			</button>
		</div>
	</div>
</aside>

<style>
	.sidebar {
		background: var(--color-surface);
		border-right: 1px solid var(--color-border);
		display: flex;
		flex-direction: column;
		gap: var(--space-5);
		padding: var(--space-4) var(--space-3);
		height: 100vh;
		position: sticky;
		top: 0;
	}
	.brand {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		padding: 0 var(--space-2);
	}
	.mark {
		color: var(--color-accent);
	}
	.name {
		font-weight: var(--font-weight-bold);
		font-size: var(--text-md);
	}
	.close-mobile {
		display: none;
		margin-left: auto;
		background: none;
		border: none;
		color: var(--color-text-muted);
		font-size: var(--text-md);
		cursor: pointer;
		padding: var(--space-1);
	}
	.backdrop {
		display: none;
	}

	.tenant {
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
	}
	.tenant-pill {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		padding: var(--space-2) var(--space-3);
		border: 1px solid var(--color-border);
		border-radius: var(--radius-sm);
		font-size: var(--text-sm);
	}
	.dot {
		width: 7px;
		height: 7px;
		border-radius: 50%;
		background: var(--color-accent);
		flex: none;
	}
	.tenant-name {
		font-weight: var(--font-weight-medium);
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.role {
		margin-left: auto;
		color: var(--color-text-muted);
		font-size: var(--text-xs);
		text-transform: uppercase;
	}
	.switch {
		font-size: var(--text-xs);
		color: var(--color-text-muted);
		padding: 0 var(--space-3);
		text-decoration: none;
	}
	.switch:hover {
		color: var(--color-accent);
	}
	.switch.signin {
		display: block;
		padding: var(--space-2) var(--space-3);
		border: 1px solid var(--color-border);
		border-radius: var(--radius-sm);
		font-size: var(--text-sm);
		color: var(--color-text);
		text-align: center;
	}

	nav {
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
	}
	nav a {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		padding: var(--space-2) var(--space-3);
		border-radius: var(--radius-sm);
		color: var(--color-text-muted);
		text-decoration: none;
		font-size: var(--text-base);
	}
	nav a:hover {
		background: var(--color-surface-raised);
		color: var(--color-text);
	}
	nav a.active {
		background: color-mix(in srgb, var(--color-accent) 14%, transparent);
		color: var(--color-text);
		font-weight: var(--font-weight-medium);
	}
	.ic {
		width: 1rem;
		text-align: center;
		opacity: 0.85;
	}

	.footer {
		margin-top: auto;
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}
	.palette-hint {
		display: flex;
		align-items: center;
		justify-content: space-between;
		width: 100%;
		background: none;
		border: 1px solid var(--color-border);
		border-radius: var(--radius-sm);
		padding: var(--space-2) var(--space-3);
		color: var(--color-text-muted);
		font-family: var(--font-ui);
		font-size: var(--text-sm);
		cursor: pointer;
	}
	.palette-hint:hover {
		border-color: var(--color-border-strong);
		color: var(--color-text);
	}
	kbd {
		font-family: var(--font-mono);
		background: var(--color-bg);
		border: 1px solid var(--color-border);
		border-radius: 3px;
		padding: 0.05rem 0.3rem;
		font-size: var(--text-xs);
	}
	.controls {
		display: flex;
		gap: var(--space-2);
	}
	.controls select,
	.density-toggle {
		flex: 1;
		height: 1.9rem;
		font-size: var(--text-xs);
		background: var(--color-bg);
		border: 1px solid var(--color-border);
		border-radius: var(--radius-sm);
		color: var(--color-text-muted);
		font-family: var(--font-ui);
		cursor: pointer;
	}
	.sr-only {
		position: absolute;
		width: 1px;
		height: 1px;
		overflow: hidden;
		clip: rect(0 0 0 0);
	}

	/* Below this width the sidebar becomes an off-canvas drawer
	   (+layout.svelte renders a menu button to open it) instead of a
	   fixed grid column -- "shouldn't break on a laptop screen or a
	   tablet in landscape" is the actual bar (this is a desktop-first
	   tool), so the breakpoint is deliberately narrower than a phone
	   viewport would need. */
	@media (max-width: 860px) {
		.sidebar {
			position: fixed;
			left: 0;
			top: 0;
			z-index: 90;
			width: 16rem;
			transform: translateX(-100%);
			transition: transform 0.15s ease;
			box-shadow: var(--shadow-lg);
		}
		.sidebar.mobile-open {
			transform: translateX(0);
		}
		.close-mobile {
			display: block;
		}
		.backdrop {
			display: block;
			position: fixed;
			inset: 0;
			background: rgba(0, 0, 0, 0.5);
			border: none;
			z-index: 80;
			padding: 0;
		}
	}
	@media (prefers-reduced-motion: reduce) {
		.sidebar {
			transition: none;
		}
	}
</style>
