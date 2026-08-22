// First real API-client module -- previously each route did its own
// inline fetch(). Introduced in Phase 3 because the surface triples
// (query + dashboards + panels + export/import); still zero-dependency,
// a thin fetch wrapper, not a generated client.

export const apiBase = import.meta.env.VITE_API_BASE_URL ?? 'http://localhost:8080';
export const alertingBase = import.meta.env.VITE_ALERTING_API_BASE_URL ?? 'http://localhost:8081';
// Optional third backend (Phase 4; enterprise/ is AGPLv3 same as core
// as of Phase 6, but stays a separate optional service architecturally)
// -- undefined in a deployment that hasn't built/deployed enterprise-auth,
// same "runtime capability check" shape /docs/phase-4-rbac-design.md's
// Web UI boundary section describes. getAuthFeatures below treats a
// missing base URL the same as a failed fetch: everything reports
// disabled, no broken links.
export const enterpriseAuthBase = import.meta.env.VITE_ENTERPRISE_AUTH_BASE_URL as string | undefined;

// Local login (see api/localauth's package doc comment). Baked in at
// build time same as the base URLs above -- requestFrom/alertingRequest
// below only send `credentials: 'include'` when this is true, since
// api's/alerting's own CORS stays the permissive wildcard-friendly
// WithCORS (no Access-Control-Allow-Credentials) unless the deployment
// set LOCAL_AUTH_ENABLED server-side too -- browsers categorically
// refuse to combine a credentialed fetch with a wildcard
// Access-Control-Allow-Origin, so sending credentials unconditionally
// would break every plain `docker compose up` local-dev deployment,
// which never sets either of these.
export const localAuthEnabled = import.meta.env.VITE_LOCAL_AUTH_ENABLED === 'true';

// Public-demo convenience: with both set at build time, the login page
// starts with these credentials already in the fields, so a visitor to a
// public demo can sign in without being handed a password out of band.
// Two separate vars, neither with a default, and the login page requires
// BOTH before prefilling anything -- a deployment that sets neither (every
// deployment except the demo) gets exactly today's empty form, and a
// half-configured one can't leave a password sitting next to an empty
// username box.
//
// This bakes a password into a static bundle, which is only acceptable
// for what it's for: a throwaway read-only Viewer account on a deployment
// whose entire database is wiped and reseeded nightly. Never point these
// at an account that can do anything worth doing.
export const demoUsername = import.meta.env.VITE_DEMO_USERNAME as string | undefined;
export const demoPassword = import.meta.env.VITE_DEMO_PASSWORD as string | undefined;

// A deployment that prints its own login credentials on its login screen
// is, by definition, the public demo -- so the same two build args also
// gate the demo notice on the landing page, rather than a third flag
// that could drift out of sync with them.
export const isPublicDemo = Boolean(demoUsername && demoPassword);

export type Language = '' | 'sql' | 'spl';

// warnings (Phase 7) is populated by the shared costguard package's
// assessment of every query, hand-written or AI-suggested alike --
// informational only, never a reason a query didn't run. Optional/absent
// (not an empty array) when there's nothing to say, matching the
// backend's `omitempty` -- see api/queryapi/handler.go's doc comment on
// why this is additive, not new enforcement.
export type QueryResult = { columns: string[]; rows: unknown[][]; warnings?: string[] };

export type VizType = 'table' | 'line' | 'bar' | 'single_stat' | 'top_n' | 'heatmap';

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
		...(localAuthEnabled ? { credentials: 'include' as RequestCredentials } : {}),
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

// ---- AI-assisted query authoring (Phase 7 Track A) ----
// Every function here returns text/suggestions only -- running anything
// still goes through runQuery above, unchanged, per the phase's
// non-negotiable "no parallel execution path" design principle. A
// deployment with no OLLAMA_BASE_URL configured has these routes
// entirely unregistered server-side (api/cmd/api's main.go), so a 404
// here is a normal, expected "AI isn't enabled" response, not a bug --
// callers (QueryBar.svelte) treat any error from these functions as
// "AI unavailable right now," never a user-facing failure.

export type Confidence = 'high' | 'medium' | 'low';

export function aiComplete(queryPrefix: string, language: string): Promise<{ suggestion: string }> {
	return request('/ai/complete', { method: 'POST', body: JSON.stringify({ queryPrefix, language }) });
}

export function aiExplain(
	query: string,
	language: string,
	originalIntent?: string
): Promise<{ explanation: string }> {
	return request('/ai/explain', {
		method: 'POST',
		body: JSON.stringify({ query, language, originalIntent: originalIntent ?? '' })
	});
}

export type FixResponse = {
	suggestedQuery: string;
	explanation: string;
	confidence: Confidence | '';
	blocked: boolean;
	costWarnings?: string[];
};

export function aiFix(
	query: string,
	language: string,
	opts: { parseError?: string; executionError?: string }
): Promise<FixResponse> {
	return request('/ai/fix', {
		method: 'POST',
		body: JSON.stringify({
			query,
			language,
			parseError: opts.parseError ?? '',
			executionError: opts.executionError ?? ''
		})
	});
}

export type OptimizeResponse = {
	findings: string[];
	phrased: string;
	suggestedQuery?: string;
};

export function aiOptimize(query: string, language: string): Promise<OptimizeResponse> {
	return request('/ai/optimize', { method: 'POST', body: JSON.stringify({ query, language }) });
}

// ---- Natural language translation (Phase 7 Track B) ----
// Same non-negotiable split as every other AI operation: this returns a
// query for review, never executes it. Running the result is the exact
// same runQuery() above, reused unchanged -- task 9's explicit
// requirement.
export type TranslateResponse = {
	query: string;
	confidence: Confidence | '';
	lowConfidenceReason?: string;
	compiles: boolean;
	compileError?: string;
	blocked: boolean;
	costWarnings?: string[];
};

export function aiTranslate(nlQuery: string): Promise<TranslateResponse> {
	return request('/ai/translate', { method: 'POST', body: JSON.stringify({ nlQuery }) });
}

// ---- Interaction audit logging (task 12) ----
// Fire-and-forget: a failure here (including AI being unconfigured
// server-side, same 404-is-normal posture as every other /ai/* call)
// must never block or surface an error for the accept/dismiss action
// that triggered it, so callers should not await rejection handling
// beyond a swallowed .catch(() => {}).
export type InteractionOperation = 'translate' | 'fix' | 'optimize';

export function logInteraction(entry: {
	operation: InteractionOperation;
	input: string;
	output: string;
	confidence?: Confidence | '';
	accepted: boolean;
	edited: boolean;
	finalQuery?: string;
}): Promise<void> {
	return request('/ai/log-interaction', {
		method: 'POST',
		body: JSON.stringify({
			operation: entry.operation,
			input: entry.input,
			output: entry.output,
			confidence: entry.confidence ?? '',
			accepted: entry.accepted,
			edited: entry.edited,
			finalQuery: entry.finalQuery ?? ''
		})
	});
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
// cairnobs_pending_login cookie enterprise-auth's finishLogin sets when an
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

export type CurrentSession = { tenant_id: string; user_id: string; role: string };

// getCurrentSession backs the sidebar's tenant indicator (Phase 5).
// POST /internal/authorize already exists (api/authz.HTTPAuthorizer and
// alerting's queryclient call it the same way) and is already reachable
// from the browser -- enterprise-auth's whole mux is wrapped in
// WithCredentialedCORS, not just the tenant-picker routes -- so this is
// a client for an existing endpoint, not a new one. Returns null for
// every "no tenant to show" case alike (no enterprise-auth configured,
// not logged in, or a plain single-tenant deployment) rather than
// throwing -- same "absence is a normal deployment shape" posture
// getAuthFeatures already uses, so the sidebar can just render nothing
// instead of branching on error types.
export async function getCurrentSession(): Promise<CurrentSession | null> {
	if (!enterpriseAuthBase) return null;
	try {
		const res = await fetch(`${enterpriseAuthBase}/internal/authorize`, {
			method: 'POST',
			credentials: 'include'
		});
		if (!res.ok) return null;
		const session = (await res.json()) as CurrentSession;
		return session.tenant_id ? session : null;
	} catch {
		return null;
	}
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

// --- local login (single-tenant mode, see api/localauth) --------------

export type LocalSession = {
	user_id: string;
	tenant_id: string;
	username: string;
	role: string;
	// The user's stored display-timezone preference (IANA name).
	// Optional: absent on deployments whose api predates the setting, and
	// on the login response, which doesn't carry it -- both mean "UTC".
	timezone?: string;
};


export function login(username: string, password: string): Promise<LocalSession & { token: string }> {
	return request('/auth/login', {
		method: 'POST',
		credentials: 'include',
		body: JSON.stringify({ username, password })
	});
}

// Stores the caller's own display-timezone preference. Display only --
// it changes nothing about what any query returns (see
// metadata/migrations/0042_add_user_display_timezone.sql). Available to
// every role, including Viewer, since it's a setting about the reader
// rather than about the data.
export function setDisplayTimezone(timezone: string): Promise<void> {
	return request('/auth/timezone', {
		method: 'PUT',
		credentials: 'include',
		body: JSON.stringify({ timezone })
	});
}

export function logout(): Promise<void> {
	return request('/auth/logout', { method: 'POST', credentials: 'include' });
}

// getLocalSession is three-state, not a boolean -- +layout.ts's route
// guard needs to tell "not logged in" (null, redirect to /login) apart
// from "this deployment doesn't have local auth turned on at all"
// ('disabled', let the request through) -- GET /auth/session is only
// ever registered server-side when LOCAL_AUTH_ENABLED is set (see
// api/localauth.Handler.RegisterRoutes' doc comment), so a 404 here
// means the latter, same "absence is a normal deployment shape" posture
// getAuthFeatures/getCurrentSession above already use for enterprise
// auth. Always sends credentials regardless of the module-level
// localAuthEnabled flag -- this is the one call the route guard makes
// unconditionally to *discover* whether local auth is on, so it can't
// rely on that flag being true first.
export async function getLocalSession(): Promise<LocalSession | 'disabled' | null> {
	try {
		const res = await fetch(`${apiBase}/auth/session`, { credentials: 'include' });
		if (res.status === 404) return 'disabled';
		if (!res.ok) return null;
		return await res.json();
	} catch {
		return null;
	}
}

export type LocalUser = { id: string; username: string; role: string; created_at: string };

export function listUsers(): Promise<LocalUser[]> {
	return request<LocalUser[]>('/auth/users', { credentials: 'include' }).then((u) => u ?? []);
}

export function createUser(username: string, password: string, role: string): Promise<LocalUser> {
	return request('/auth/users', {
		method: 'POST',
		credentials: 'include',
		body: JSON.stringify({ username, password, role })
	});
}

export function deleteUser(id: string): Promise<void> {
	return request(`/auth/users/${id}`, { method: 'DELETE', credentials: 'include' });
}

// resetPassword's response only carries `password` when the caller
// didn't supply one -- see api/localauth/handler.go's
// resetPasswordResponse doc comment.
export function resetPassword(id: string, newPassword?: string): Promise<{ password?: string }> {
	return request(`/auth/users/${id}/reset-password`, {
		method: 'POST',
		credentials: 'include',
		body: JSON.stringify(newPassword ? { password: newPassword } : {})
	});
}

export function setUserRole(id: string, role: string): Promise<LocalUser> {
	return request(`/auth/users/${id}/role`, {
		method: 'PUT',
		credentials: 'include',
		body: JSON.stringify({ role })
	});
}

// changeOwnPassword is the only way to change your own password (any
// role, including owner) -- distinct from resetPassword above, which
// is exclusively for an owner/admin acting on someone ELSE's account
// and will reject a request that targets the caller's own id. Revokes
// the caller's own session on success (see api/localauth/handler.go's
// handleChangeOwnPassword), so the caller must sign in again afterward.
export function changeOwnPassword(currentPassword: string, newPassword: string): Promise<void> {
	return request('/auth/password', {
		method: 'POST',
		credentials: 'include',
		body: JSON.stringify({ current_password: currentPassword, new_password: newPassword })
	});
}

// --- log retention (owner/admin only, see api/logretention) -----------
// Deletion is scoped to specific (host, service) targets, not wholesale
// -- a caller must name which agents' *and* which log types' logs to
// target (listRetentionHosts is how the UI discovers what to offer,
// grouped by host with each host's services underneath), and
// api/logretention never treats an omitted target list as "everything."

export type HostService = { host: string; service: string };
export type BlockedTarget = { host: string; service: string; protected_days: number };
export type RetentionService = { service: string; count: number; protected_days?: number };
export type RetentionHost = { host: string; protected_days?: number; services: RetentionService[] };
export type RetentionHostsResult = { hosts: RetentionHost[]; cutoff: string };
export type LogRetentionPreview = {
	count: number;
	cutoff: string;
	targets: HostService[];
	blocked_targets?: BlockedTarget[];
};
export type LogRetentionDeleteResult = {
	deleted_count: number;
	cutoff: string;
	deleted_targets: HostService[];
	blocked_targets?: BlockedTarget[];
};

// The `?? []` fallbacks below are a second line of defense, matching
// listUsers' pattern above -- api/logretention/handler.go now always
// sends `[]` rather than `null` for these fields (see its partitionTargets
// and handleHosts comments), but a null response should degrade to an
// empty list here rather than crash the page's `.length` accesses if
// that guarantee ever regresses.
export function listRetentionHosts(olderThanHours: number): Promise<RetentionHostsResult> {
	return request<RetentionHostsResult>(`/logs/retention/hosts?older_than_hours=${olderThanHours}`, {
		credentials: 'include'
	}).then((r) => ({ ...r, hosts: r.hosts ?? [] }));
}

export function previewLogDeletion(olderThanHours: number, targets: HostService[]): Promise<LogRetentionPreview> {
	return request<LogRetentionPreview>('/logs/retention/preview', {
		method: 'POST',
		credentials: 'include',
		body: JSON.stringify({ older_than_hours: olderThanHours, targets })
	}).then((r) => ({ ...r, targets: r.targets ?? [] }));
}

export function deleteLogsOlderThan(olderThanHours: number, targets: HostService[]): Promise<LogRetentionDeleteResult> {
	return request<LogRetentionDeleteResult>('/logs/retention/delete', {
		method: 'POST',
		credentials: 'include',
		body: JSON.stringify({ older_than_hours: olderThanHours, targets })
	}).then((r) => ({ ...r, deleted_targets: r.deleted_targets ?? [] }));
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

// ---- Agent inventory + remote config ----
// See /docs/agent-management-design.md. An agent only appears here
// after it's checked in at least once (GET /agents/{host} 404s until
// then) -- there's no "pre-register a host" step, inventory is purely
// observed from real check-ins.
export type ConfigOverride = {
	batch_max_size?: number;
	batch_flush_interval_ms?: number;
	heartbeat_enabled?: boolean;
	heartbeat_interval_ms?: number;
	journald_unit?: string;
	extra_file_paths?: string[];
	// log_retention_days is owner-only to change (see api/agents/handler.go's
	// changesLogRetentionDays) -- it's a central policy tag api/logretention
	// reads as a protective floor, not something the agent process itself
	// ever sees or applies.
	log_retention_days?: number;
	// service_log_retention_days is log_retention_days' per-service
	// refinement, also owner-only -- a service present here overrides
	// log_retention_days for that service only; every other service on
	// this host still falls back to log_retention_days.
	service_log_retention_days?: Record<string, number>;
};

export type Agent = {
	id: string;
	tenant_id: string;
	host: string;
	service: string;
	agent_version: string;
	source_kind: string;
	source_detail: string;
	batch_max_size: number;
	batch_flush_interval_ms: number;
	heartbeat_enabled: boolean;
	heartbeat_interval_ms: number;
	first_seen_at: string;
	last_seen_at: string;
	desired_override?: ConfigOverride;
	desired_override_version?: string;
	applied_override_version: string;
	pending: boolean;
	updated_by?: string;
	// pending_command/command_issued_at/by (real, restart only for now
	// -- see /docs/agent-management-design.md's punch list) have no
	// "delivered" signal to show the way config's `pending` does:
	// ingest clears pending_command the instant it hands the command to
	// the agent, not once the agent confirms it ran, so
	// command_issued_at is shown as a last-issued record instead.
	pending_command?: string;
	command_issued_at?: string;
	command_issued_by?: string;
};

export function listAgents(): Promise<Agent[]> {
	return request<Agent[]>('/agents').then((a) => a ?? []);
}

export function getAgent(host: string): Promise<Agent> {
	return request(`/agents/${encodeURIComponent(host)}`);
}

export function setAgentConfig(host: string, override: ConfigOverride): Promise<Agent> {
	return request(`/agents/${encodeURIComponent(host)}/config`, {
		method: 'PUT',
		body: JSON.stringify(override)
	});
}

export function clearAgentConfig(host: string): Promise<void> {
	return request(`/agents/${encodeURIComponent(host)}/config`, { method: 'DELETE' });
}

// "restart" is the only supported value today -- server-validated
// (400 on anything else), RoleAdmin-gated (403 for a Viewer/Editor
// session), and audit-logged. See /docs/agent-management-design.md's
// punch list for why stop/uninstall aren't here yet.
export function issueAgentCommand(host: string, command: 'restart'): Promise<Agent> {
	return request(`/agents/${encodeURIComponent(host)}/command`, {
		method: 'PUT',
		body: JSON.stringify({ command })
	});
}

// ---- Host CPU/memory/disk metrics ----
// No new REST endpoints -- a metrics sample is an ordinary log record
// (see agent/README.md's "Host CPU/memory/disk metrics" section and
// agent/cairnobs-agent/src/main.rs's send_metrics), tagged
// `cairnobs.metrics=true`, fetched through the same POST /query every
// other page already uses via runQuery(). Only ever set on one agent
// process per physical host, so `stats count by host` over this tag
// naturally lists real hosts, not every fragmented per-source agent
// identity `/agents` shows (see the deployment notes on why several
// agent processes can share one physical host under different
// `[agent] host` values).

export type HostSummary = { host: string; sampleCount: number };

export async function listMetricsHosts(): Promise<HostSummary[]> {
	const result = await runQuery('cairnobs.metrics=true | stats count by host', 'spl');
	const hostIdx = result.columns.indexOf('host');
	const countIdx = result.columns.indexOf('count');
	return result.rows.map((r) => ({ host: String(r[hostIdx]), sampleCount: Number(r[countIdx]) }));
}

export type HostMetrics = {
	host: string;
	timestamp: string;
	cpuPercent: number;
	memUsedBytes: number;
	memTotalBytes: number;
	diskUsedBytes: number;
	diskTotalBytes: number;
	// Static-or-slow-changing context (see agent/src/metrics.rs's
	// Metrics doc comment) -- sent on the same record specifically so a
	// viewer never has to correlate two different samples to make sense
	// of the utilization numbers above (is 21% CPU busy or idle depends
	// on core count; is this usage normal depends on uptime).
	cpuCores: number;
	osName: string;
	kernelVersion: string;
	arch: string;
	uptimeSeconds: number;
	ipv4Addresses: string[];
	ipv6Addresses: string[];
};

// Reads straight out of the record's `attributes` object (already
// returned in full on every query result row) rather than trying to
// project attribute-derived fields as top-level query-language columns
// -- simpler, and doesn't depend on `fields` supporting synthetic
// attribute columns the same way filtering does.
export async function getHostMetrics(host: string): Promise<HostMetrics | null> {
	const result = await runQuery(
		`host="${host}" cairnobs.metrics=true | sort -timestamp | head 1`,
		'spl'
	);
	if (result.rows.length === 0) return null;
	const row = result.rows[0];
	const timestampIdx = result.columns.indexOf('timestamp');
	const attributesIdx = result.columns.indexOf('attributes');
	const attrs = (row[attributesIdx] ?? {}) as Record<string, string>;
	const num = (key: string) => Number(attrs[key] ?? 0);
	// Comma-joined by the agent (see agent/src/main.rs's send_metrics) --
	// split back into a list here, filtering out the empty string a
	// host with no addresses of a given family produces (''.split(',')
	// is [''], not [], so the filter is load-bearing, not defensive).
	const addrList = (key: string) => (attrs[key] ?? '').split(',').filter((a) => a !== '');
	return {
		host,
		timestamp: String(row[timestampIdx]),
		cpuPercent: num('cpu_percent'),
		memUsedBytes: num('mem_used_bytes'),
		memTotalBytes: num('mem_total_bytes'),
		diskUsedBytes: num('disk_used_bytes'),
		diskTotalBytes: num('disk_total_bytes'),
		cpuCores: num('cpu_cores'),
		osName: attrs['os_name'] ?? 'unknown',
		kernelVersion: attrs['kernel_version'] ?? 'unknown',
		arch: attrs['arch'] ?? 'unknown',
		uptimeSeconds: num('uptime_seconds'),
		ipv4Addresses: addrList('ipv4_addresses'),
		ipv6Addresses: addrList('ipv6_addresses')
	};
}
