import { get, SignedOut, type Topology } from './api';
import { live } from './live.svelte';

// Both map pages poll the same endpoint.
export function topology() {
	const s = $state<{ data: Topology | null; error: string }>({ data: null, error: '' });
	async function load() {
		try {
			s.data = await get<Topology>('/api/v1/topology');
			s.error = '';
		} catch (e) {
			if (e instanceof SignedOut) void live.checkSession();
			else s.error = String(e);
		}
	}
	$effect(() => {
		void load();
		const t = setInterval(() => void load(), 15000);
		return () => clearInterval(t);
	});
	return s;
}
