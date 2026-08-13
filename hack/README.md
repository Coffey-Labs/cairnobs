# hack

Local developer tooling that isn't part of any shipped component — scripts
you run against your own machine/dev stack, not code that ends up in a
container image (except `dev-certs`' *output*, which mounts into the
ingest container).

Not one of the top-level directories in the original monorepo scaffold —
added because dev-only mTLS cert generation didn't have a natural home in
`/deploy` (real deployment manifests), `/transport`, or any other existing
component. `/hack` is the conventional name for this in a lot of larger Go
monorepos (Kubernetes among them).

- `dev-certs/` — generates a throwaway CA + server/client cert pair for
  local mTLS between the agent and ingest. See `/docs/phase-0-runbook.md`
  for when to run it.
- `windows-fixture/` — sends synthetic Windows Event Log-shaped records
  directly to `ingest`, bypassing the real Windows agent. Tests whether
  the pipeline handles Windows-shaped data; doesn't test the real
  `EvtSubscribe`/ETW integration, which needs actual Windows. See
  `/docs/phase-1-runbook.md`.
