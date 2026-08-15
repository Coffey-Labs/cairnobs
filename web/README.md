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
docker build -f Dockerfile -t sentry-web .   # context is web/, not the repo root
docker run -p 3000:3000 sentry-web
```

## Tenant picker (Phase 4)

`src/routes/select-tenant` is the one route that isn't reachable by
clicking around the app -- `enterprise-auth`'s `internal/loginhandler`
redirects a browser here after an SSO login resolves to more than one
`tenant_memberships` row (see that package's doc comment), carrying a
short-lived `sentry_pending_login` cookie instead of a real session. The
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
placeholder page justifies. `nginx.conf` here is minimal: serve `build/`,
fall back to `index.html` for client-side routing (only one route exists
today, but this is what you want the moment a second one is added).
