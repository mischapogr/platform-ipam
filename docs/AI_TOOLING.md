# Project agent tooling

Migrated to native Codex configuration and skill discovery on 2026-09-08 for the AWS-focused platform-ipam project. [AGENTS.md](../AGENTS.md) routes tasks to five small [project skills](../.agents/skills/ipam-contract/SKILL.md). Main application plans and examples retain their own implementation status.

## Start a focused Codex session

```bash
scripts/ai/codex --check-config
scripts/ai/codex --check-skills
scripts/ai/codex
```

The authoritative project configuration is [.codex/config.toml](../.codex/config.toml). Codex loads it natively in a trusted checkout, including when launched directly with `codex -C /path/to/platform-ipam`. The [launcher](../scripts/ai/codex) selects this checkout and forwards the caller's arguments unchanged. It supplies no MCP configuration overrides and does not rewrite global configuration, credentials, model, sandbox, or approval settings. Unrelated project settings can coexist in the native file. The former invocation overlay has been removed.

`--check-config` starts a fresh Codex app-server, reads `config/read` with layers, and requires this checkout's active project layer. It compares every declared MCP setting, including the AWS endpoint and Terraform image/arguments, with the effective configuration. It also runs native `codex mcp list --json` to validate registrations and flags. No `-c` overrides are used. A missing/disabled project layer cannot pass even if inherited flags happen to match. This checks configuration loading, not server connectivity.

`--check-skills` uses `skills/list` with `forceReload` in a fresh app-server process. It requires each of the five named skills exactly once, enabled, with repository scope and its expected native path. Neither diagnostic starts a model turn or calls MCP tools. Run diagnostics alone; combining them with command-line overrides is rejected so overrides cannot conceal a native-loading failure.

`--print-config` prints only project server names, enabled flags, and whether the transport is declared locally or inherited. It omits commands, arguments, environments, headers, endpoint values, and unrelated configuration. Diagnostics suppress raw inherited configuration and errors because they may contain credentials.

The project enables AWS Knowledge, Terraform's public registry toolset, Context7, and GitHub. Context7 and GitHub retain their inherited transports and credentials; on a new machine those registrations must first exist. The named disabled entries are `phpocalypse`, `astro-app-mysql`, `astro-schmid-shop-mysql`, `swiss-ephemeris`, `immanuel-astrology`, `stitch`, `git-local`, `tmux`, `semgrep`, `playwright`, and `chrome-devtools`. Inherited disabled `symfony-ai-mate` and `1password` entries remain untouched. Unknown future servers and hosted apps are not disabled automatically.

Enable Playwright for a UI task with an explicit one-off override:

```bash
scripts/ai/codex -c 'mcp_servers.playwright.enabled=true'
```

The Terraform MCP uses the published Docker image `hashicorp/terraform-mcp-server:1.2.0`. Its first use needs a permitted Docker daemon and registry access; pulling by a pinned release tag is not the same as a digest lock. AWS Knowledge requires outbound DNS/HTTPS. NetBox MCP is deferred until a development NetBox endpoint and scoped read token exist; no placeholder live connection is installed.

## Native project skills and managed access

The five maintained folders now live only under [.agents/skills](../.agents/skills): `ipam-contract`, `aws-reconciliation`, `terraform-provider`, `compose-development`, and `helm-validation`. They were moved from the former top-level directory, without duplicate definitions. Skill document links resolve through `../../../docs/`; command examples are run from the repository root. AGENTS.md routes to the native paths, and `check-contract` scans Markdown under `.agents/skills`.

The normal managed sandbox returns `EROFS` (`Read-only file system`, errno 30) when writing `.codex` or `.agents`. The session's normal approval mechanism permitted both write probes and the scoped migration. Ordinary sandbox access remains read-only; later edits to these directories require equivalent approved access. No global Codex settings or trust entries were changed; this checkout was already trusted.

Fresh app-server diagnostics also need access to Codex's runtime state directory. In this sandbox, startup fails with `failed to initialize sqlite state runtime under /home/mp/.codex`; approved execution succeeds. The launcher reports this as incomplete verification (exit 2), never as a configuration pass. Approval for diagnostics allows normal Codex runtime bookkeeping, not global configuration edits.

Start a fresh Codex session to activate the project's MCP configuration. The already-running session retains its original MCP registrations. Skill discovery was verified in a fresh app-server; restart also refreshes any session that has not picked up the moved skills.

## Scoped checks

All scripts require Python 3.11 or later. YAML example syntax additionally uses PyYAML. Provider formatting requires Terraform >=1.6,<2.0, matching the examples. Other tools are detected at invocation; missing required artifacts/tools are reported instead of downloaded or silently skipped.

```bash
scripts/ai/check-contract
scripts/ai/check-provider
scripts/ai/check-provider --plan-json /path/to/terraform-plan.json
scripts/ai/check-compose
scripts/ai/check-compose --env-file /path/to/local-dev.env
scripts/ai/check-helm
```

| Command | Implemented scope | Separate evidence still required |
| --- | --- | --- |
| `check-contract` | Local Markdown file links, JSON/YAML example syntax, OpenAPI validator when a schema/tool exists | Anchor/remote link checks, example/schema conformance, breaking-change comparison, API runtime tests |
| `check-provider` | Terraform example formatting, short provider Go tests when the module exists, optional replacement-key plan guard | Initialized provider schema checks, full state/import/retry/AWS acceptance tests |
| `check-compose` | Quiet configuration validation for base, NetBox, and development override combinations that exist | Startup, readiness, seed, persisted-data and restart recovery |
| `check-helm` | Strict lint and local rendering for both stage/prod values | Kubernetes API/schema compatibility, workload identity, secret access, migrations, rollout |

Exit codes: `0` means checks within the stated local scope passed; `1` means a check failed; `2` means a required artifact/tool or environment capability is missing. `NOT_CHECKED` entries name work outside that local scope. An overall static pass is not a deployment or lifecycle sign-off. Today missing OpenAPI/provider/Compose/chart implementations make the relevant commands return `2`.

Each command emits one compact JSON summary including duration and log paths. Full logs are in a new private `platform-ipam-*` directory under the system temporary directory. Logs can contain rendered configuration or test output; treat them as local artifacts, not conversation attachments. No helper prints environment variables or writes Terraform state. Check commands never run `terraform init/apply`, enable `TF_ACC`, start Compose, install Helm releases, or invoke AWS provisioning.

Provider unit checks honor an existing `GOCACHE`; otherwise they reuse a per-user `platform-ipam-go-cache-*` directory under the system temporary directory. Repeated checks do not deliberately discard compilation cache. Dependency/module caches retain the Go toolchain's configured behavior.

The optional plan guard rejects same-key/unknown-key allocation replacements and explicitly incomplete/deferred plans. It is a narrow rule, not a full Terraform plan authorization engine. Preserve the source plan as an access-controlled artifact because Terraform JSON can contain sensitive values.

## Allocation diagnostics

```bash
# Saved API response: no network or credentials needed.
scripts/ai/inspect-allocation alloc_01 --input /path/to/allocation.json

# Read-only GET using PLATFORM_IPAM_URL and PLATFORM_IPAM_TOKEN.
scripts/ai/inspect-allocation alloc_01
```

The helper requires HTTPS; explicit local development HTTP is limited to loopback via `PLATFORM_IPAM_ALLOW_LOCAL_HTTP=1`. It rejects redirects, mismatched IDs, operation responses, and documents over 1 MiB. It prints selected allocation fields and preserves unavailable evidence as unknown. It never decides reuse or makes independent AWS/NetBox queries; the server's reclamation gates remain authoritative.

## Validation and efficiency

Run `python3 -B scripts/ai/test_helpers.py` after changing helper behavior. Its tests exercise replacement safety, missing implementations, diagnostic identity/uncertainty, and launcher argument handling without contacting production or changing the main application.

Compare representative contract, provider, and diagnosis tasks using elapsed time, token/tool-output volume, retries, and correctness. The helpers expose duration and concise evidence; no token-saving percentage has been measured. Keep detailed reference material in the existing API/client/AWS/deployment documents and load it only for the task.

## Verified results (2026-09-08)

Verification used Codex CLI `0.153.4`. Configuration loading, skill discovery, and MCP tool execution were tested separately:

| Verification | Result and evidence |
| --- | --- |
| Protected directory access | Normal sandbox: `.codex` and `.agents` both returned errno 30 / EROFS. Approved write probes and migration succeeded. |
| Skill validation | All five moved skills passed the installed skill-creator `quick_validate.py` validator (exit 0 each). Descriptions and workflow instructions were preserved; only relative document links changed. |
| Helper tests | `python3 -B scripts/ai/test_helpers.py`: 22 tests passed. Coverage includes native-layer absence/disabled trust, matching flags with incorrect transport settings, inheritance, secret-safe diagnostics, literal CLI forwarding, no obsolete fallback, discovery duplicates/disabled skills/wrong paths, and native skill-link scanning. |
| Native configuration | Approved `scripts/ai/codex --check-config`: PASSED, active native project layer present, zero mismatches. AWS Knowledge, Terraform, Context7, and GitHub enabled; all 11 named disabled entries matched, and inherited disabled 1Password/Symfony entries stayed disabled. No launcher MCP overrides. |
| Configuration preservation | Parsed native project MCP settings equal the former overlay. Every pre-existing inherited transport compares equal before/after, including credentials. The global Codex config SHA-256 was unchanged. No model, sandbox, approval, or trust settings were edited. |
| Fresh skill discovery | Approved `scripts/ai/codex --check-skills`: PASSED. Fresh app-server `skills/list` returned all five exactly once, enabled, scope `repo`, at `.agents/skills/<name>/SKILL.md`; zero discovery errors. No extra search roots or model turn. |
| Document links and examples | `scripts/ai/check-contract`: zero broken local file links, JSON/YAML syntax passed. Overall **BLOCKED**, exit 2: OpenAPI implementation is missing. This is not an application validation pass. |
| AWS Knowledge connectivity | Initial sandbox DNS resolution failed. Approved native-transport MCP initialization/discovery succeeded; `aws___search_documentation` for “Amazon VPC subnet CIDR blocks” returned the AWS “Subnet CIDR blocks” documentation. |
| Terraform MCP connectivity | Initial sandbox Docker socket access was denied. With approval, Docker was accessible; the missing pinned `hashicorp/terraform-mcp-server:1.2.0` image was downloaded. Native-transport discovery exposed nine registry tools only. `get_latest_provider_version(namespace="hashicorp", name="aws")` returned `6.63.0`. The temporary MCP containers exited; a filtered Docker check found none running. |
| Context7 connectivity | Existing session MCP `resolve_library_id` for Python 3.11 TOML parsing returned `/python/cpython` with version `v3.11.14` available. Its inherited native transport was verified unchanged. |
| GitHub connectivity | Existing session MCP `get_file_contents` against public `hashicorp/terraform-mcp-server`, ref `v1.2.0`, failed with **Authentication Failed: Bad credentials**. The inherited native transport and credentials were preserved. Authentication remains unresolved; no credential replacement or login was attempted. |

The GitHub owner must repair the inherited credential through their normal credential management flow, then repeat a harmless read-only MCP request. Native configuration loading alone does not establish GitHub connectivity. Approved network/Docker access was available for these probes; it does not remove restrictions from ordinary sandbox commands.

Application/runtime checks beyond the reported static contract scope were not run. Missing provider/Compose/Helm implementations are not counted as successful checks. No commit, deployment, AWS provisioning, or application-service startup was performed. Restart the active Codex session to use the native project MCP registrations.

Private local probe artifacts were kept under `/tmp/platform-ipam-mcp-*`; only bounded status information was reported. They are temporary evidence, not maintained configuration or a skill-installation fallback.

References: [Codex native configuration](https://learn.chatgpt.com/docs/config-file/config-basic), [Codex MCP configuration](https://learn.chatgpt.com/docs/extend/mcp?surface=cli), [Codex app-server inspection](https://learn.chatgpt.com/docs/app-server), [Codex skill discovery](https://learn.chatgpt.com/docs/build-skills), [AWS Knowledge MCP](https://awslabs.github.io/mcp/servers/aws-knowledge-mcp-server), [Terraform MCP release](https://github.com/hashicorp/terraform-mcp-server/releases/tag/v1.2.0), [OpenAPI validator](https://openapi-spec-validator.readthedocs.io/en/latest/), [Compose config](https://docs.docker.com/reference/cli/docker/compose/config/), [Helm lint](https://helm.sh/docs/helm/helm_lint/).
