import adapter from '@sveltejs/adapter-static';
import { sveltekit } from '@sveltejs/kit/vite';
import { defineConfig } from 'vite';

export default defineConfig({
	plugins: [
		sveltekit({
			compilerOptions: {
				// Force runes mode for the project, except for libraries. Can be removed in svelte 6.
				runes: ({ filename }) => filename.split(/[/\\]/).includes('node_modules') ? undefined : true
			},
			// fallback: dashboards/[id] and alerts/[id] are dynamic routes
			// whose params (dashboard/rule IDs) don't exist at build time, so
			// they can't be prerendered like the root/list pages. Named
			// 200.html, not index.html -- the root route ("/") is itself
			// prerendered to index.html, and a same-named fallback silently
			// overwrites it (confirmed by actually running the build: "Using
			// index.html" produced the warning "Overwriting build/index.html
			// with fallback page"). nginx.conf's try_files points at
			// /200.html to match.
			adapter: adapter({ fallback: '200.html' })
		})
	]
});
