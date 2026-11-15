# Security policy

## Supported versions

Security fixes go into the latest release. Older releases don't get
backported fixes; upgrading is a package install, and both the server and
the agents keep their state across it.

| Version | Supported |
| --- | --- |
| Latest release | Yes |
| Older releases | No |

## Reporting a vulnerability

**Please don't open a public issue for a security problem.**

Email **security@coffeylabs.org** with:

- what the problem is and what someone could do with it
- how to reproduce it
- the versions involved (`obs-beacond -version`, `obs-beacon-agent -version`)
- whether it's in the server, the agent, the root helper, the cluster
  agent, or the web interface

You'll hear back within a few days. We'll agree a date for the fix and its
disclosure with you, and credit you if you'd like.

## Scope

In scope:

- Anything that lets someone change what Cairn OBS is meant only to read:
  running commands through an agent or its root helper, a certificate
  reload or renewal the host's own config doesn't allow, or writing through
  any read-only account it's given
- Agent identity: one host reporting as another, enrolling without a valid
  token, or getting past the agent CA's pinning
- Sign-in and roles: getting in without a matching rule, or a viewer or
  operator doing what only an admin may
- Secrets: anything sealed with the state key, or kept in a `*_file`,
  showing up in the web interface, the API, logs or errors
- Internet clients' addresses leaving the host they were seen on

Out of scope, and best reported to their makers:

- Vulnerabilities in the systems Cairn OBS reads (Kubernetes, Vault, Proxmox
  and the rest) or in your identity provider
