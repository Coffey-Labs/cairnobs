import adapter from '@sveltejs/adapter-static';
import { vitePreprocess } from '@sveltejs/vite-plugin-svelte';

/** @type {import('@sveltejs/kit').Config} */
export default {
	preprocess: vitePreprocess(),
	kit: {
		// A single-page app: obs-beacond serves index.html for every page path.
		adapter: adapter({ fallback: 'index.html' })
	}
};
