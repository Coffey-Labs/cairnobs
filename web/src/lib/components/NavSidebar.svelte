<script lang="ts">
	import { page } from '$app/state';
	import {
		getCurrentSession,
		enterpriseAuthBase,
		localAuthEnabled,
		getLocalSession,
		logout,
		type CurrentSession,
		type LocalSession
	} from '$lib/api';
	import { getTheme, setTheme, type Theme } from '$lib/theme.svelte';
	import { getDensity, toggleDensity } from '$lib/density.svelte';

	let {
		onOpenPalette,
		mobileOpen = false,
		onCloseMobile
	}: { onOpenPalette: () => void; mobileOpen?: boolean; onCloseMobile?: () => void } = $props();

	const baseNavItems = [
		{ href: '/', label: 'Search', icon: '◇' },
		{ href: '/dashboards', label: 'Dashboards', icon: '▤' },
		{ href: '/alerts', label: 'Alerts', icon: '▲' },
		{ href: '/data-sources', label: 'Data Sources', icon: '◈' },
		{ href: '/agents', label: 'Agents', icon: '●' },
		{ href: '/hosts', label: 'Hosts', icon: '▣' }
	];
	const usersNavItem = { href: '/users', label: 'Users', icon: '◐' };
	const settingsNavItem = { href: '/settings', label: 'Settings', icon: '⚙' };

	function isActive(href: string): boolean {
		if (href === '/') return page.url.pathname === '/';
		return page.url.pathname.startsWith(href);
	}

	let session: CurrentSession | null = $state(null);
	$effect(() => {
		getCurrentSession().then((s) => (session = s));
	});

	let localSession: LocalSession | null = $state(null);
	$effect(() => {
		if (!localAuthEnabled) return;
		getLocalSession().then((s) => (localSession = s === 'disabled' ? null : s));
	});

	// The Users nav item only ever makes sense for local-auth mode's
	// owner/admin user manager (see routes/users/+page.svelte, which
	// admin can now partially use too -- viewer/editor accounts only) --
	// an enterprise-SSO deployment or a plain viewer/editor local
	// session never sees it, same gating that page enforces itself if
	// reached directly.
	const canManageUsers = $derived.by(() => {
		const s = localSession;
		return s !== null && (s.role === 'owner' || s.role === 'admin');
	});
	const navItems = $derived(
		canManageUsers ? [...baseNavItems, usersNavItem, settingsNavItem] : [...baseNavItems, settingsNavItem]
	);

	let loggingOut = $state(false);
	async function handleLogout() {
		loggingOut = true;
		try {
			await logout();
		} finally {
			window.location.href = '/login';
		}
	}

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
	{:else if localAuthEnabled && localSession}
		<div class="tenant">
			<div class="tenant-pill">
				<span class="dot" aria-hidden="true"></span>
				<span class="tenant-name">{localSession.username}</span>
				<span class="role">{localSession.role}</span>
			</div>
			<a class="switch" href="/account">Change password</a>
			<button type="button" class="switch logout-btn" onclick={handleLogout} disabled={loggingOut}>
				{loggingOut ? 'Signing out…' : 'Log out'}
			</button>
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
	.logout-btn {
		background: none;
		border: none;
		font-family: var(--font-ui);
		width: 100%;
		text-align: left;
		cursor: pointer;
	}
	.logout-btn:disabled {
		cursor: default;
		opacity: 0.6;
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
