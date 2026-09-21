# ADR 0005: Apache-2.0, public release, and the limits of controlling reuse

Status: accepted, 2026-09-08

## Context

The project is moving from a private working tree to a public GitHub
repository. Two requirements were stated: the code must be usable both inside
proprietary systems and by other open-source projects, and reuse should
ideally be constrained so the work is not simply taken and diverged.

Those requirements are partly incompatible, and the incompatibility is not
negotiable through wording.

GitHub does not offer a way to disable forking on a public repository. The
"Allow forking" control exists only for private repositories and, at
organization level, for private and internal ones. GitHub's Terms of Service
grant every user the right to view and fork public repositories; publishing is
the act of granting that right. There is no repository setting, licence text,
or file that changes this while the repository is public.

A licence cannot close the gap either. Any licence that forbade redistribution
or derivative works would fail the Open Source Definition, would make the code
unusable by the open-source projects it is meant to serve, and would still not
prevent the fork button from working.

The repository also carries specific legal surface: it ships a Terraform
provider binary and a Go library, both of which downstream consumers embed
rather than merely run.

## Decision

The project is licensed under the Apache License 2.0, and the repository is
public with forking accepted as a consequence of publishing.

Apache-2.0 rather than MIT because of what this repository distributes. It
grants an explicit patent licence from contributors, which matters for a
project implementing address-allocation and reconciliation logic that consumers
embed. It carries an explicit trademark reservation, so the project name is not
implicitly licensed with the code. Its contribution clause states that
contributions are under the same terms without requiring a separate agreement.
It is also the licence most infrastructure and Terraform-ecosystem consumers
already have cleared for internal use, which removes a review step for exactly
the audience this project targets.

A `NOTICE` file carries the copyright attribution and records the independently
licensed components the project integrates with but does not redistribute.
Per-file licence headers are deliberately not added: `LICENSE` and `NOTICE`
satisfy Apache-2.0, and headers on every file are noise that drifts.

Control over reuse is therefore exercised through the things that actually
work, not through prohibition: being the canonical source, documenting the
reasoning behind decisions so a fork inherits the "why" and not only the
"what", and keeping the contribution path cheaper than maintaining a
divergence.

Both Go modules are renamed to `github.com/mischapogr/platform-ipam` and
`github.com/mischapogr/platform-ipam/providers/terraform`. The previous paths
were `platform-ipam` — not a resolvable URL — and an organization that does not
exist, so neither module could be fetched by an external consumer.

## Consequences

Anyone may use, modify, redistribute, and fork this project, including inside
closed commercial products, provided they retain the licence and notices. That
is the deliberate outcome of the stated goal that both proprietary and
open-source consumers can use it. Forks cannot be revoked or deleted by the
maintainer.

Because a published fork is permanent, what ships in the first public commit
matters more than any subsequent cleanup. Local agent configuration, editor
state, and MCP logs — which named unrelated private projects — are excluded
from the repository rather than removed later.

The patent grant binds contributors as well as the maintainer, which is a
higher bar for a casual contributor than MIT and may deter a drive-by change.
That is accepted: a contribution to allocation logic warrants the clarity.

Renaming the modules is a breaking change for any existing import path. No
external consumer exists yet, so it is free now and would not be later; leaving
it would have made the public repository un-`go get`-able, which is worse.

If controlling reuse ever outweighs public availability, the only mechanism
that actually enforces it is to stop being public: a private repository with
forking disabled, or an enterprise-owned internal repository. That is a
visibility decision, not a licensing one, and reversing publication does not
retract forks already taken.
