import { sveltekit } from '@sveltejs/kit/vite';
import { defineConfig } from 'vite';

// `npm run dev` serves the UI on :5180 and passes the API to a local obs-beacond.
export default defineConfig({
	plugins: [sveltekit()],
	server: {
		port: 5180,
		strictPort: true,
		proxy: { '/api': 'http://127.0.0.1:8150', '/auth': 'http://127.0.0.1:8150' }
	}
});
