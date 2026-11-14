// Passkeys in the browser: obs-beacond sends WebAuthn options as JSON with
// base64url fields; the browser API wants ArrayBuffers, and back again.

const toBuf = (s: string) => {
	const b = atob(s.replace(/-/g, '+').replace(/_/g, '/') + '='.repeat((4 - (s.length % 4)) % 4));
	return Uint8Array.from(b, (c) => c.charCodeAt(0)).buffer;
};
const toB64 = (b: ArrayBuffer | null) => {
	if (!b) return null;
	let s = '';
	for (const c of new Uint8Array(b)) s += String.fromCharCode(c);
	return btoa(s).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
};

export const passkeysSupported = () => typeof window !== 'undefined' && !!window.PublicKeyCredential;

async function call(url: string, body?: unknown): Promise<any> {
	const r = await fetch(url, {
		method: 'POST', credentials: 'same-origin',
		headers: { 'X-Beacon': '1', 'Content-Type': 'application/json' },
		body: body === undefined ? undefined : typeof body === 'string' ? body : JSON.stringify(body)
	});
	const text = await r.text();
	let data: any = null;
	try { data = text ? JSON.parse(text) : null; } catch { /* not JSON */ }
	if (!r.ok) throw new Error(data?.error ?? `${r.status} ${text}`);
	return data;
}

// The browser says "not allowed" when someone cancels the prompt.
function plain(e: unknown): Error {
	if (e instanceof DOMException && (e.name === 'NotAllowedError' || e.name === 'AbortError')) return new Error('Cancelled, or no passkey was chosen.');
	if (e instanceof DOMException && e.name === 'InvalidStateError') return new Error('This device already has a passkey for this account.');
	return e instanceof Error ? e : new Error(String(e));
}

export async function signInWithPasskey(): Promise<void> {
	const opts = await call('/auth/passkey/begin');
	const pk = opts.publicKey;
	pk.challenge = toBuf(pk.challenge);
	pk.allowCredentials = (pk.allowCredentials ?? []).map((c: any) => ({ ...c, id: toBuf(c.id) }));
	let cred: PublicKeyCredential;
	try {
		cred = (await navigator.credentials.get({ publicKey: pk })) as PublicKeyCredential;
	} catch (e) {
		throw plain(e);
	}
	const r = cred.response as AuthenticatorAssertionResponse;
	await call('/auth/passkey/finish', {
		id: cred.id, rawId: toB64(cred.rawId), type: cred.type,
		response: { clientDataJSON: toB64(r.clientDataJSON), authenticatorData: toB64(r.authenticatorData), signature: toB64(r.signature), userHandle: toB64(r.userHandle) }
	});
}

export interface SetupInfo { name: string; email: string; role: string; emergency?: boolean; passkeys?: boolean; passwords?: boolean; secure?: boolean }

export async function setupInfo(token: string): Promise<SetupInfo> {
	return call('/auth/setup/info', { token });
}

export async function registerPasskey(token: string, name: string): Promise<void> {
	const opts = await call('/auth/setup/begin', { token });
	const pk = opts.publicKey;
	pk.challenge = toBuf(pk.challenge);
	pk.user.id = toBuf(pk.user.id);
	pk.excludeCredentials = (pk.excludeCredentials ?? []).map((c: any) => ({ ...c, id: toBuf(c.id) }));
	let cred: PublicKeyCredential;
	try {
		cred = (await navigator.credentials.create({ publicKey: pk })) as PublicKeyCredential;
	} catch (e) {
		throw plain(e);
	}
	const r = cred.response as AuthenticatorAttestationResponse;
	await call('/auth/setup/finish?name=' + encodeURIComponent(name), {
		id: cred.id, rawId: toB64(cred.rawId), type: cred.type,
		response: { clientDataJSON: toB64(r.clientDataJSON), attestationObject: toB64(r.attestationObject), transports: r.getTransports?.() ?? [] }
	});
}

// A name for the passkey that a person will recognise in the Users list.
export function deviceName(): string {
	const ua = navigator.userAgent;
	const os = /iPhone|iPad/.test(ua) ? 'iPhone or iPad' : /Android/.test(ua) ? 'Android' : /Mac/.test(ua) ? 'Mac' : /Windows/.test(ua) ? 'Windows' : /Linux/.test(ua) ? 'Linux' : 'This device';
	const br = /Edg\//.test(ua) ? 'Edge' : /Firefox\//.test(ua) ? 'Firefox' : /Chrome\//.test(ua) ? 'Chrome' : /Safari\//.test(ua) ? 'Safari' : '';
	return br ? `${os} · ${br}` : os;
}
