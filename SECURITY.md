# Security policy

## Reporting a vulnerability

**Do not open a public issue for a security problem.**

Report privately through GitHub Security Advisories:
<https://github.com/mischapogr/platform-ipam/security/advisories/new>

That channel is private to the maintainer until an advisory is published, and
it lets you be credited when it is.

Please include what you can: affected version or commit, a description of the
class of problem, the impact you believe it has, and the minimum needed to
reproduce it. A proof of concept is welcome but not required — do not include
a working exploit against a system you do not own.

You should get an acknowledgement within 7 days. This is a personal project
with no commercial support attached, so treat any timeline beyond that
acknowledgement as best effort rather than a commitment.

## Supported versions

The project has not reached a stable release. Only the default branch receives
fixes; there is no backport branch and no security support for tagged
pre-release versions.

| Version | Supported |
| --- | --- |
| `main` | Yes |
| everything else | No |

## Scope

This repository holds an IP address allocation service, its NetBox and AWS
adapters, a Terraform provider, and a local development stack.

**In scope**, and genuinely interesting for this system:

- Anything that lets one tenant read, modify, or release another tenant's
  allocation, or reserve address space in an account, environment, or region
  the authenticated principal is not authorized for.
- Anything that lets a client control tenancy, pool selection, or the chosen
  CIDR — those are server-side decisions by design.
- Anything that causes two live allocations to overlap, or that makes held
  address space appear reusable without complete observation evidence.
- Credential handling: a bearer token that leaks into logs, diagnostics,
  Terraform state, an error body, or across a redirect or origin.
- Idempotency defects that turn one logical request into two allocations.

**Out of scope:**

- The `deploy/compose/` development stack, which is deliberately insecure. It
  ships known-weak defaults, publishes to loopback only, and refuses to run
  outside `IPAM_ENVIRONMENT=development`. Weak development credentials are the
  documented design, not a finding.
- Anything requiring a compromised developer machine or Docker daemon.
- Missing hardening in example configuration under `examples/`, which is
  illustrative and labelled as such.
- Vulnerabilities in NetBox, PostgreSQL, Terraform, or the AWS SDK themselves.
  Report those upstream; tell us only if this project's usage makes an upstream
  issue exploitable in a way it otherwise would not be.

## Handling of the development stack

`create-env.sh` generates local credentials and refuses to overwrite an
existing `.env`. Those credentials are for a disposable local environment.
Never reuse them anywhere else, and never point the development stack at a
staging or production NetBox.
