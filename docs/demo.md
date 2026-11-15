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

One container with no network at all (`--network none`) holds both
obs-beacond and the simulator, so nothing in it can reach anything else:

- obs-beacond listens on a unix socket in a folder the reverse proxy can
  reach (`listen: unix:/run/demo/web.sock`, formats.md). The agent API isn't
  on that socket.
- The simulator's agents report to a loopback agent listener
  (`agent_listen: 127.0.0.1:8461`, `agent_tls: {off: true}`), and its
  stand-ins for Cloudflare, Akamai, mail, Vault, Patroni and the alert
  channels answer on the container's own loopback.
- `fleet config -demo` writes the server's pieces; `fleet run -demo` runs
  the fleet (add `-ibmi 4` for IBM i and the HMC). Start the fleet first so
  its stand-ins are up for obs-beacond's first readings.
- The root is read-only and the state is in tmpfs, so restarting the
  container is a fresh demo; restart it on a timer (weekly is plenty) so
  history doesn't pile up.
