# Contributing to platform-ipam

Thanks for your interest. This project allocates real network address space,
so the bar for changes is correctness first and convenience second.

By contributing you agree that your contributions are licensed under the
[Apache License 2.0](LICENSE).

## Before you start

Read [AGENTS.md](AGENTS.md) for the working boundaries, and the decision
records in [`docs/decisions/`](docs/decisions/) for why the system is shaped
the way it is. If your change contradicts a decision record, say so in the
issue — changing the decision is allowed, changing it silently is not.

For anything beyond a typo, open an issue first. A design discussion before
code is cheaper than a rejected pull request.

## Setting up

You need Docker and Docker Compose. You do **not** need Go or Terraform
installed: both run from pinned images.

```sh
cd deploy/compose && ./create-env.sh && cd ../..
./tests/e2e/run-e2e.sh
```

That builds the stack, bootstraps NetBox, resets allocation state, and runs the
end-to-end suite. See [`tests/e2e/README.md`](tests/e2e/README.md).

## Running the checks

```sh
# Unit tests, in the pinned image.
docker run --rm -v "$PWD":/src:ro -v ipam-gomod:/go/pkg/mod -w /src \
  golang:1.26.8-bookworm go test ./...

scripts/ai/check-contract    # documentation links, examples, OpenAPI
scripts/ai/check-compose     # Compose combinations and NetBox secret lengths
scripts/ai/check-provider    # Terraform formatting and provider unit tests
scripts/ai/check-helm        # chart lint and render

./tests/e2e/run-e2e.sh       # the full end-to-end suite
```

These emit compact JSON. **Exit code 2 means BLOCKED — verification did not
happen.** It is not a pass. Read the referenced log before continuing.

## What a change needs

- **A test that fails without it.** Prefer an end-to-end test for anything that
  touches allocation, lifecycle, or an adapter. Several defects in this
  repository passed every unit test and only failed against a running stack.
- **No silent failure.** An error that is swallowed, or a generic `503` that
  discards its cause, is treated as a defect in its own right.
- **No new allocation policy in a client.** Clients are transport only. The API
  owns pool selection, CIDR choice, and tenancy.
- **Documentation updated in the same change.** If behaviour changes, the
  relevant document under `docs/` changes with it.

## File naming

Markdown files use upper snake case (`docs/ONBOARDING_IMPORT.md`). Decision records keep their number and a hyphen first: `docs/decisions/0007-ONBOARDING_IMPORT_AS_UNMANAGED_OCCUPANCY.md`. Update every link in the same change; `scripts/ai/check-contract` fails on a broken one.

## Commit messages

[Conventional Commits](https://www.conventionalcommits.org) 1.0.0:

```
<type>(<optional scope>)<!>: <subject>

<body: why, wrapped at 100>

<footers: BREAKING CHANGE: ..., Refs: #123>
```

Types: `feat` `fix` `docs` `refactor` `perf` `test` `build` `ci` `chore`
`style` `revert`. Subject in the imperative mood, lowercase, no trailing
period, header line at most 72 characters. Release automation reads this
history, so a non-conforming message drops out of the changelog.

## Pull requests

Keep them focused; a reviewer should be able to hold the whole change in mind.
State what you verified and how, and be explicit about what you did **not**
verify — live AWS, a Kubernetes rollout, and provider distribution are not
covered by any check in this repository.

Forks are how contributions work here. If you are maintaining a long-lived
divergence rather than proposing a change back, that is your right under the
licence; an issue describing what is missing upstream is still appreciated.

## Security

Do not open a public issue for a vulnerability. Follow [SECURITY.md](SECURITY.md).
