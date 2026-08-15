// First real API-client module -- previously each route did its own
// inline fetch(). Introduced in Phase 3 because the surface triples
// (query + dashboards + panels + export/import); still zero-dependency,
// a thin fetch wrapper, not a generated client.

export const apiBase = import.meta.env.VITE_API_BASE_URL ?? 'http://localhost:8080';
export const alertingBase = import.meta.env.VITE_ALERTING_API_BASE_URL ?? 'http://localhost:8081';
// Optional third backend (Phase 4, commercial-license) -- undefined in a
// deployment that hasn't built/deployed enterprise-auth, same "runtime
// capability check" shape /docs/phase-4-rbac-design.md's Web UI boundary
// section describes. getAuthFeatures below treats a missing base URL the
// same as a failed fetch: everything reports disabled, no broken links.
export const enterpriseAuthBase = import.meta.env.VITE_ENTERPRISE_AUTH_BASE_URL as string | undefined;

export type Language = '' | 'sql' | 'spl';

export type QueryResult = { columns: string[]; rows: unknown[][] };

export type VizType = 'table' | 'line' | 'bar' | 'single_stat' | 'top_n';

export type Panel = {
	id: string;
	dashboard_id: string;
	title: string;
	query: string;
	query_language: Language;
	viz_type: VizType;
	viz_config: Record<string, string>;
	position_x: number;
	position_y: number;
	width: number;
	height: number;
	earliest_override: string | null;
	latest_override: string | null;
	sort_order: number;
};

export type Dashboard = {
	id: string;
	name: string;
	description: string;
	default_earliest: string;
	default_latest: string;
	created_at: string;
	updated_at: string;
	panels: Panel[] | null;
};

class ApiError extends Error {}

async function requestFrom<T>(base: string, path: string, init?: RequestInit): Promise<T> {
	const res = await fetch(`${base}${path}`, {
		headers: { 'Content-Type': 'application/json' },
		...init
	});
	if (!res.ok) {
		let message = `request failed with status ${res.status}`;
		try {
			const body = await res.json();
			if (body?.error) message = body.error;
		} catch {
			// non-JSON error body -- keep the generic message
		}
		throw new ApiError(message);
	}
	if (res.status === 204) return undefined as T;
	return res.json();
}

function request<T>(path: string, init?: RequestInit): Promise<T> {
	return requestFrom(apiBase, path, init);
}

// alerting is a separate service (its own base URL) -- see
// /docs/phase-3-alerting-design.md's component boundary.
function alertingRequest<T>(path: string, init?: RequestInit): Promise<T> {
	return requestFrom(alertingBase, path, init);
}

export function runQuery(query: string, language: Language): Promise<QueryResult> {
	return request('/query', { method: 'POST', body: JSON.stringify({ query, language }) });
}

export function listDashboards(): Promise<Dashboard[]> {
	return request('/dashboards').then((d) => (d as Dashboard[]) ?? []);
}

export function getDashboard(id: string): Promise<Dashboard> {
	return request(`/dashboards/${id}`);
}

export function createDashboard(input: {
	name: string;
	description?: string;
	default_earliest?: string;
	default_latest?: string;
}): Promise<Dashboard> {
	return request('/dashboards', { method: 'POST', body: JSON.stringify(input) });
}

export function updateDashboard(
	id: string,
	input: { name: string; description?: string; default_earliest?: string; default_latest?: string }
): Promise<Dashboard> {
	return request(`/dashboards/${id}`, { method: 'PUT', body: JSON.stringify(input) });
}

export function deleteDashboard(id: string): Promise<void> {
	return request(`/dashboards/${id}`, { method: 'DELETE' });
}

export function addPanel(dashboardId: string, panel: Partial<Panel>): Promise<Panel> {
	return request(`/dashboards/${dashboardId}/panels`, { method: 'POST', body: JSON.stringify(panel) });
}

export function updatePanel(dashboardId: string, panel: Partial<Panel>): Promise<Panel> {
	return request(`/dashboards/${dashboardId}/panels/${panel.id}`, {
		method: 'PUT',
		body: JSON.stringify(panel)
	});
}

export function deletePanel(dashboardId: string, panelId: string): Promise<void> {
	return request(`/dashboards/${dashboardId}/panels/${panelId}`, { method: 'DELETE' });
}

export type AuthFeatures = { sso_configured: boolean; oidc_enabled: boolean; saml_enabled: boolean };

// getAuthFeatures never throws -- an absent/unreachable enterprise-auth
// is a normal, expected deployment shape (Phase 0-3-style single-tenant,
// no enterprise/ deployed), not an error condition the settings page
// should surface. Deliberately no `credentials: 'include'` here: this
// endpoint needs no session, and CORS_ALLOWED_ORIGIN's dev default of
// "*" can't be combined with credentialed fetches anyway (the browser
// rejects that combination) -- the same reason api.ts's other requests
// don't send credentials yet. Wiring session cookies through is part of
// the deferred login-flow work, alongside tightening CORS_ALLOWED_ORIGIN
// to a real origin, done together as one change so neither breaks the
// other.
export async function getAuthFeatures(): Promise<AuthFeatures> {
	const disabled = { sso_configured: false, oidc_enabled: false, saml_enabled: false };
	if (!enterpriseAuthBase) return disabled;
	try {
		const res = await fetch(`${enterpriseAuthBase}/auth/features`);
		if (!res.ok) return disabled;
		return await res.json();
	} catch {
		return disabled;
	}
}

// --- tenant picker (Phase 4) -------------------------------------------
//
// The two calls below are the reason getAuthFeatures above doesn't send
// credentials but these do: they carry the short-lived
// sentry_pending_login cookie enterprise-auth's finishLogin sets when an
// identity resolves to more than one tenant_memberships row (see
// enterprise/internal/loginhandler's package doc comment), and
// selectTenant's response sets the real session cookie. Both require
// `credentials: 'include'`, which is exactly why enterprise-auth's CORS
// (httpserver.WithCredentialedCORS) can't use getAuthFeatures'/api.ts's
// other requests' wildcard-friendly posture -- browsers refuse to honor
// Access-Control-Allow-Origin: "*" on a credentialed request at all, so
// CORS_ALLOWED_ORIGIN has to name this page's real origin.

export type Membership = { tenant_id: string; tenant_display_name: string; role: string };

class TenantPickerError extends Error {}

// enterprise-auth's loginhandler responds to an error with plain
// http.Error text (e.g. "no membership in the requested tenant"), not a
// JSON {"error": "..."} body the way /api's queryapi/dashboards handlers
// do -- requestFrom's JSON-body error parsing wouldn't surface that
// message, so this reads the body as plain text instead.
async function enterpriseAuthRequest<T>(path: string, init?: RequestInit): Promise<T> {
	if (!enterpriseAuthBase) {
		throw new TenantPickerError('enterprise-auth is not configured (VITE_ENTERPRISE_AUTH_BASE_URL unset)');
	}
	const res = await fetch(`${enterpriseAuthBase}${path}`, {
		credentials: 'include',
		headers: { 'Content-Type': 'application/json' },
		...init
	});
	if (!res.ok) {
		const body = await res.text();
		throw new TenantPickerError(body || `request failed with status ${res.status}`);
	}
	return res.json();
}

// listMemberships backs the tenant-picker page's initial load -- see
// web/src/routes/select-tenant. A 400/401 (missing or expired pending
// login) surfaces as a thrown TenantPickerError; the page's own error
// state is what tells the user to start over at login.
export function listMemberships(): Promise<Membership[]> {
	return enterpriseAuthRequest('/auth/memberships');
}

export function selectTenant(tenantId: string): Promise<{ redirect_url: string }> {
	return enterpriseAuthRequest('/auth/select-tenant', {
		method: 'POST',
		body: JSON.stringify({ tenant_id: tenantId })
	});
}

export function exportDashboard(id: string): Promise<Dashboard> {
	return request(`/dashboards/${id}/export`);
}

export function importDashboard(dashboard: Dashboard): Promise<Dashboard> {
	return request('/dashboards/import', { method: 'POST', body: JSON.stringify(dashboard) });
}

// resolveTimeRange applies the override-or-default rule from
// /docs/phase-3-dashboard-design.md's "Time-range mechanics": a panel's
// own earliest/latest override wins if set, otherwise the dashboard's
// default applies.
export function resolveTimeRange(
	dashboard: Pick<Dashboard, 'default_earliest' | 'default_latest'>,
	panel: Pick<Panel, 'earliest_override' | 'latest_override'>
): { earliest: string; latest: string } {
	return {
		earliest: panel.earliest_override ?? dashboard.default_earliest,
		latest: panel.latest_override ?? dashboard.default_latest
	};
}

// injectTimeRange prepends earliest=/latest= as leading base_search
// terms -- works because they're ordinary implicit-AND terms in Phase
// 2's grammar, order-independent. Never used for raw-SQL panels (the
// dashboards API rejects query_language: "sql" on panels entirely, so
// this never has to handle that case).
//
// "now" is a UI-only sentinel (the default_latest value shown in the
// time-range picker), not a token the query language understands --
// time_expr only accepts a quoted absolute timestamp or a "-N unit"
// relative offset (see /docs/query-language-design.md). Emitting a
// literal `latest=now` produces a real compile error ("expected a
// quoted absolute timestamp or a relative offset"), caught by actually
// running this against the live stack. Omitting the latest= clause
// entirely is the query language's own way of saying "no upper bound",
// which is exactly what "now" means here.
export function injectTimeRange(query: string, earliest: string, latest: string): string {
	const clauses = [`earliest=${earliest}`];
	if (latest && latest !== 'now') clauses.push(`latest=${latest}`);
	return `${clauses.join(' ')} ${query}`;
}

// --- alerting ---------------------------------------------------------

export type ConditionType = 'threshold' | 'absence';
export type Comparator = 'gt' | 'gte' | 'lt' | 'lte' | 'eq' | 'ne';
export type NotificationKind = 'webhook' | 'slack' | 'pagerduty';
export type AlertRuleState = 'ok' | 'pending' | 'firing';

export type NotificationTarget = {
	id: string;
	name: string;
	kind: NotificationKind;
	webhook_url: string;
};

export type AlertRule = {
	id: string;
	name: string;
	description: string;
	query: string;
	query_language: Language;
	condition_type: ConditionType;
	comparator?: Comparator;
	threshold_value?: number;
	eval_interval_seconds: number;
	for_minutes: number;
	renotify_interval_minutes?: number;
	notification_target_id: string;
	enabled: boolean;
	state: {
		state: AlertRuleState;
		last_evaluated_at?: string;
		last_eval_status: 'ok' | 'error';
		last_error?: string;
		last_value?: number;
		consecutive_errors: number;
	};
};

export type DeliveryLogEntry = {
	id: number;
	event_type: 'firing' | 'resolved';
	status: 'pending' | 'sent' | 'failed' | 'retrying';
	attempt_count: number;
	last_error?: string;
	response_status?: number;
	created_at: string;
};

export function listRules(): Promise<AlertRule[]> {
	return alertingRequest<AlertRule[]>('/rules').then((r) => r ?? []);
}

export function getRule(id: string): Promise<AlertRule> {
	return alertingRequest(`/rules/${id}`);
}

export function createRule(input: Partial<AlertRule>): Promise<AlertRule> {
	return alertingRequest('/rules', { method: 'POST', body: JSON.stringify(input) });
}

export function deleteRule(id: string): Promise<void> {
	return alertingRequest(`/rules/${id}`, { method: 'DELETE' });
}

export function listDeliveries(ruleId: string): Promise<DeliveryLogEntry[]> {
	return alertingRequest<DeliveryLogEntry[]>(`/rules/${ruleId}/deliveries`).then((d) => d ?? []);
}

export function listNotificationTargets(): Promise<NotificationTarget[]> {
	return alertingRequest<NotificationTarget[]>('/targets').then((t) => t ?? []);
}

export function createNotificationTarget(input: {
	name: string;
	kind: NotificationKind;
	webhook_url: string;
}): Promise<NotificationTarget> {
	return alertingRequest('/targets', { method: 'POST', body: JSON.stringify(input) });
}
