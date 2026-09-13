# Security

Persea Terminal is designed for a trusted Tailscale ingress and a single
operator identity. Do not expose its Unix front-door socket through an untrusted
reverse proxy or public listener: the entire header-based identity chain assumes
the only process that can open that socket is the pinned reverse proxy, and peer
uid verification is what enforces it.

## Reporting a vulnerability

Do not open a public issue. Use GitHub's **private vulnerability reporting** on
this repository (Security → Report a vulnerability), which is the supported
channel and requires no prior contact.

Include the affected version, the configuration shape, reproduction steps, and the
impact you believe it has. Never include live capability handles, Tailscale keys,
cookies, or terminal contents — a report should be reproducible from its
description, not from captured secrets.

Expect an acknowledgement within a week. This is a small project maintained
without a security team, so please allow reasonable time for a fix before any
public disclosure.

## Scope notes for reporters

Two properties are **by design** and not vulnerabilities:

- **A single operator identity reaches every realm.** Realms isolate unix users,
  not people. See the single-operator note in the README.
- **Control mode is arbitrary code execution as the realm's unix user.** That is
  what a terminal is. The security boundary is who may obtain Control, not what
  they can then run.

Supported deployments use the strict host manifest, isolated tagged Tailscale
Service identity, exact operator login, AF_UNIX peer credentials, and the
packaged verification scripts. A successful local install is not proof of
remote reachability or tailnet policy.

The production ingress contract matches the pinned Tailscale Serve adapter:
after the root peer and exact forwarded-host checks, plaintext port 80 has no
`X-Forwarded-Proto` header and is redirect-only, while application traffic must
carry exactly one `X-Forwarded-Proto: https`. Explicit `http`, duplicate,
comma-joined, empty, or other values fail closed. The hermetic local adapter is
separate and requires exactly one `X-Forwarded-Proto: http` by default.
Explicit hermetic TLS uses `https` instead and remains restricted to the
non-root, same-user loopback configuration; it is not a production ingress mode.
Application responses set one-year exact-host HSTS without
`includeSubDomains`.

Secrets belong outside the repository. The deployment scripts do not accept a
Tailscale auth key argument, and browser attachment handles are short-lived,
single-purpose capabilities that must not appear in logs or reports.
