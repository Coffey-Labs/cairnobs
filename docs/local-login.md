# Local login

Username and password authentication, served by `api` itself against the
control-plane Postgres. It is the alternative to Phase 4's SSO
(`enterprise-auth`, OIDC/SAML) for a deployment that wants a login screen
without an identity provider behind it.

The two are mutually exclusive by construction. `cmd/api/main.go` picks
one authorizer: `ENTERPRISE_AUTH_URL` wins if it is set, and only
otherwise does `LOCAL_AUTH_ENABLED` take effect. With SSO configured,
`/auth/*` is never registered and local login does not exist.

## Off by default, and why

A plain `docker compose up` has no authentication at all. That is
deliberate rather than an oversight: every Phase 0-3 runbook verifies the
pipeline with bare `curl` against `/query`, and those steps only work
against an unauthenticated API. Turning login on globally would break the
project's own documented verification procedure.

So it is an opt-in overlay:

```sh
docker compose -f docker-compose.yml -f docker-compose.local-auth.yml up -d --build
```

`--build` is not optional. One of the four settings is a Vite build arg
baked into the static bundle, so a restart alone leaves the frontend
disagreeing with the backend.

## Creating the first account

There is no sign-up. The first account is an operator action:

```sh
docker compose -f docker-compose.yml -f docker-compose.local-auth.yml \
  run --rm api -seed-admin
```

```
created default admin user:
  username: admin
  password: <generated>
this password will not be shown again -- save it now.
```

It creates `admin` as an **owner** with a random password, prints it once,
and stores only a bcrypt hash. Losing it means resetting it, not
recovering it. The command is idempotent: if the `admin` account already
exists it says so and does nothing.

Then sign in at <http://localhost:3000/login>. Change the password from
the account page.

## The four settings, and why they must agree

| Setting | Service | What it does |
|---|---|---|
| `LOCAL_AUTH_ENABLED` | `api`, `alerting` | Registers `/auth/*`, and swaps `WithCORS` for `WithCredentialedCORS` |
| `CORS_ALLOWED_ORIGIN` | `api`, `alerting` | Must be a literal origin — the default `*` is refused by browsers for credentialed requests |
| `LOCAL_AUTH_COOKIE_SECURE` | `api` | Defaults to `true`; the cookie is not stored over plain `http://localhost` unless this is `false` |
| `VITE_LOCAL_AUTH_ENABLED` | `web` (build arg) | Without it the bundle never sends `credentials: 'include'` |

Only the first is obviously about login, and every one of them fails
differently:

- `LOCAL_AUTH_ENABLED` unset — the login page posts to `/auth/login` and
  gets a **404**. The route is not registered at all, deliberately, so a
  deployment with the feature off answers as though it does not exist
  rather than advertising a disabled feature.
- `CORS_ALLOWED_ORIGIN` left at `*` — the browser refuses the request
  before it is sent, and the page reports a network failure rather than
  an HTTP status.
- `LOCAL_AUTH_COOKIE_SECURE` left at `true` over HTTP — login returns
  `200` and appears to work, then every subsequent request is
  unauthenticated, because the cookie was never stored.
- `VITE_LOCAL_AUTH_ENABLED` unset — same symptom as the previous one, and
  from a different cause: the cookie exists but is never attached.

Set `LOCAL_AUTH_COOKIE_SECURE` back to `true` for anything served over
HTTPS, and set `CORS_ALLOWED_ORIGIN` to the real web origin. The values in
the overlay assume `http://localhost:3000`.

## Roles

`owner`, `admin`, `editor`, `viewer`, from `api/authz`. Owners manage
everyone; admins may create and delete `viewer`/`editor` accounts only,
and cannot reset an owner's password. Two guards prevent a deployment
from becoming unadministrable: the last owner can be neither deleted nor
demoted, and no account can delete itself while signed in.

## Not covered by the Helm chart

`deploy/helm/cairnobs` has no local-login support — it sets none of these
variables. A Kubernetes deployment authenticates via `enterprise-auth`
SSO or not at all. This is a gap, not a decision.
