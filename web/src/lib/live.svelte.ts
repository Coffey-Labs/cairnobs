import { get, SignedOut, type Overview, type Session, type SigninOptions } from './api';

/** App-wide state: who is signed in, and the overview every page shares. */
class Live {
	session = $state<Session | null>(null);
	signin = $state<SigninOptions['signin'] | null>(null);
	overview = $state<Overview | null>(null);
	error = $state('');
	updated = $state(0);
	#timer: ReturnType<typeof setInterval> | undefined;

	/** Admins change setup. */
	get admin() {
		return this.session?.role === 'admin';
	}
	/** Admins and operators see everything: log lines, the audit log and setup (read-only for operators). */
	get seesAll() {
		return this.session?.role === 'admin' || this.session?.role === 'operator';
	}

	async checkSession() {
		try {
			this.session = await get<Session>('/api/v1/session');
			this.signin = null;
		} catch (e) {
			this.session = null;
			if (e instanceof SignedOut) this.signin = e.options.signin;
			else this.error = String(e);
		}
	}

	async refresh() {
		try {
			this.overview = await get<Overview>('/api/v1/overview');
			this.error = '';
			this.updated = Date.now();
		} catch (e) {
			if (e instanceof SignedOut) {
				this.session = null;
				this.signin = e.options.signin;
				this.stop();
			} else this.error = 'Can’t reach obs-beacond: ' + String(e);
		}
	}

	start(everyMs = 5000) {
		this.stop();
		void this.refresh();
		this.#timer = setInterval(() => void this.refresh(), everyMs);
	}

	stop() {
		clearInterval(this.#timer);
		this.#timer = undefined;
	}
}

export const live = new Live();
