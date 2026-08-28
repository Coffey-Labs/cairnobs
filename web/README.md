# web

SvelteKit frontend. Phase 0: one page, one query box, one table. No auth,
no styling polish, no routing beyond `/`.

## What it does

Textarea for a raw SQL string → `POST {VITE_API_BASE_URL}/query` on `/api`
→ renders `{columns, rows}` as an HTML table, or shows `{error}` from a
rejected/failed query. That's the whole app — see `src/routes/+page.svelte`.

## Why a static build, not a Node server

Scaffolded with `@sveltejs/adapter-static`: this page has no server-side
data loading (all data comes from a client-side `fetch` triggered by the
submit button), so there's nothing here that needs a running SvelteKit
server. A prerendered static site is simpler to build, deploy, and reason
about than running Node in production for a page that's this thin.

Because it's static, `VITE_API_BASE_URL` is baked in at **build time**, not
read at container start. Set it before `npm run build` (or pass
`--build-arg VITE_API_BASE_URL=...` to `docker build`) — changing it later
means rebuilding, not just restarting the container.

## Timestamps and the display timezone

Everything in Cairn OBS is UTC: ingest records Unix nanoseconds,
ClickHouse stores UTC, and every API response is RFC3339 with a Z. The
web UI renders those instants in whichever zone the reader picked
(Settings → Display timezone), which changes **presentation only** --
never which rows a query returns, never their order, and never what
`earliest=`/`latest=` mean. Two people in two zones looking at one log
line see the same instant written two ways.

- `src/lib/time.ts` -- pure formatting. `formatTimestamp` is the one
  entry point; it detects timestamps by value, not by column name, since
  query output is arbitrary. Sub-second digits are copied verbatim from
  the source string rather than round-tripped through a JS `Date`, which
  is millisecond-precision and would silently drop the last six digits of
  a ClickHouse nanosecond timestamp.
- `src/lib/timezone.svelte.ts` -- where the choice is *stored*, which
  differs by deployment on purpose: per named user server-side when local
  login is on (`PUT /auth/timezone`, so it follows a person across
  browsers), per browser session on a public demo (a shared account's
  visitors shouldn't inherit each other's settings), per browser
  otherwise.
- Charts format their own axis and tooltip labels through the same
  helper. ECharts' `type: 'time'` axis otherwise renders in the
  *browser's* zone with no way to override it, which would put a chart's
  clock out of step with the table beside it.

Query *input* follows the same setting: an absolute time written without
an offset (`earliest=2026-08-22 09:00`) is read as wall-clock time in the
reader's zone and converted to UTC before the query is sent -- see
`src/lib/querytime.ts`. Anything with an explicit offset is taken at its
word, and relative ranges (`-24h`) never depended on a zone.

That conversion happens before a query is sent *or saved*, so a stored
dashboard range is an explicit instant rather than "10am, whoever you
are" -- otherwise one shared dashboard would show two different windows
to two people. The API itself is unchanged and still accepts only quoted
RFC3339 with an offset; everything this adds is a form it rejects today,
so no query that works now can change meaning.

## Building & running

```sh
npm install
cp .env.example .env   # adjust VITE_API_BASE_URL if /api isn't on localhost:8080
npm run dev             # local dev server with hot reload
npm run check            # svelte-check, type errors
npm run build             # static output to build/
npm run preview            # serve the static build locally to sanity-check it
```

```sh
docker build -f Dockerfile -t cairnobs-web .   # context is web/, not the repo root
docker run -p 3000:3000 cairnobs-web
```

## Tenant picker (Phase 4)

`src/routes/select-tenant` is the one route that isn't reachable by
clicking around the app -- `enterprise-auth`'s `internal/loginhandler`
redirects a browser here after an SSO login resolves to more than one
`tenant_memberships` row (see that package's doc comment), carrying a
short-lived `cairnobs_pending_login` cookie instead of a real session. The
page calls `GET /auth/memberships` to list the choices, and
`POST /auth/select-tenant` on a click, both via
`fetch(..., {credentials: 'include'})` (`$lib/api.ts`'s
`listMemberships`/`selectTenant`) so that cookie -- and, on success, the
real session cookie the POST response sets -- actually cross the origin
boundary between this app and `enterprise-auth`. `enterprise-auth`'s
default `POST_LOGIN_REDIRECT_URL` (this app's own base URL) is also
where `CORS_ALLOWED_ORIGIN` defaults to, and it has to be a literal
origin, not `*` -- see `api/httpserver.WithCredentialedCORS`'s doc
comment for why a credentialed `fetch` and a wildcard CORS origin can
never be combined; `getAuthFeatures` above deliberately doesn't send
credentials for exactly this reason, and is why it could stay on the
plain `WithCORS` every other endpoint in this repo uses.

Like every other route (`export const prerender = true` in this route's
own `+page.ts`), no server-side data loading -- the membership list and
the tenant choice both come from client-side `fetch` calls the same way
the root query page's does.

Verified in a real browser in this environment: a throwaway Node server
standing in for `enterprise-auth` (implementing the exact
`GET /auth/memberships`/`POST /auth/select-tenant` wire contract,
including the plain-text `http.Error` bodies the real handler sends, not
JSON) on a different origin/port than this app's dev server, driven
through the full flow -- cross-origin pending-login cookie set, the
credentialed preflight + `GET`/`POST` round trip, a real click choosing
a tenant, and the post-selection redirect landing back on `/` -- plus
the missing/expired-cookie error path separately. No Docker or live
Postgres/IdP needed for this, since the whole point was exercising this
app's own fetch/CORS/cookie wiring against a contract-accurate fake, not
`enterprise-auth`'s internals (those are `enterprise/internal/
loginhandler`'s own tests' job, already covered there).

## Why nginx, not distroless

The repo convention prefers distroless/scratch base images. Serving a
static SPA still needs *some* HTTP server, though, and `nginx:alpine` is
the boring, standard choice for that job — writing a custom static-file
binary just to stay distroless would be more engineering than a Phase 0
placeholder page justifies. `nginx.conf` serves `build/`, and resolves a
request in this order: the file itself, then the flat `<route>.html`
adapter-static prerenders each route to, then — for the handful of routes
that have no file on disk — the `200.html` SPA shell.

### 404s

Anything that matches none of the above answers **404**, not 200. That
took explicit work, because the natural static-SPA config falls back to
the shell unconditionally and hands every junk URL a success status;
crawlers, uptime checks and vulnerability scanners then can't tell a real
page from a miss. The 404 still *renders* the shell, so a human sees the
app's own not-found page exactly as before — only the status line
changed.

Two kinds of route legitimately have no file to match and so are named
explicitly in `nginx.conf`: dynamic routes (`dashboards/[id]` and
friends), whose params don't exist at build time, and routes that never
opted into prerendering (`/data-sources`, which has no `+page.ts`). Those
two allowlists are the only thing here that can drift out of sync with
`src/routes` — and drift would break *only production*, since `vite dev`
and `npm run preview` route from the client manifest and never read
`nginx.conf`. `hack/check-web-routes.sh` fails CI when they disagree; run
it after adding a dynamic or non-prerendered route.

Trailing slashes redirect (308) to the canonical no-slash form rather
than 404ing, matching SvelteKit's default `trailingSlash: 'never'`.
