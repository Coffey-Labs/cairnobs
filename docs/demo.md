# Running a public demo

A demo is obs-beacond watching a simulated fleet (`scripts/fleet`), with
**demo sign-in** turned on so anyone can look around without an account.

```yaml
demo_signin: true
```

With it on, the sign-in page offers **Look around**. A demo visitor:

- sees everything an admin sees, Configure included, so the demo shows the
  whole product;
- changes nothing: obs-beacond answers every request from a demo session
  that isn't a GET or HEAD with 403 "This is the demo: it shows everything
  and changes nothing." Signing out is the one exception;
- isn't written to the audit log (a public demo's visitors would bury
  everything else in it). The session shows a **Demo** badge in the header.

The people who run the demo stay private: a visitor sees made-up accounts
on Configure → Users and an empty audit log. Exports in Configure never
include channel secrets, demo or not. Notes and acknowledgements you add
while signed in as yourself carry your name, as they do anywhere.

Turning `demo_signin` off ends every demo session at its next check.

## Never on a real fleet

Demo sign-in lets in anyone who can reach the page. Use it only on an
obs-beacond whose agents are the simulator. The sign-in providers you
configure still work beside it, so you can sign in as yourself to change the
demo's settings.

## Shape of a demo host

- obs-beacond on a loopback address behind your reverse proxy, with a
  fresh database of its own.
- The simulator's agents report to a loopback agent listener. The proxy
  doesn't pass `/api/v1/agent/` at all, so nobody outside can send reports.
- `fleet config -demo` writes the server and agent config;
  `fleet run -demo` runs the fleet (add `-ibmi 4` for IBM i and the HMC).
- Reset it nightly by stopping obs-beacond, dropping its database and
  starting both again, so the demo always starts from the same story.
