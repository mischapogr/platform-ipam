# platform-ipam agent guide

This project targets AWS VPC/subnet allocation. Start with [docs/README.md](docs/README.md), then read only the contract or component needed for the task. Check the working tree before editing; preserve other in-progress work.

## Token-efficient mode

- Keep Fast mode disabled. Do not enable `/fast` or change model, inference, or speed settings unless the user explicitly requests it.
- Act immediately when the task is clear. If uncertain, inspect the minimum necessary context first.
- Be concise. Do not restate the request, explain routine actions, or write a long plan before acting. Give brief progress updates only when they convey a meaningful finding, decision, or blocker.
- Search and read only task-relevant files. Do not reread inspected files without a reason; prefer targeted searches over broad repository scans.
- Make the smallest correct change. Run contract-required and change-relevant checks, repeating them only when needed. Report unrelated failures without investigating them unless they block required verification.
- Browse only when the task requires current or source-backed information. Load only task-relevant skills and tools.
- Keep the final response to one to five short bullets when practical, covering the change, verification, and any material limit. Skip a recap when it adds nothing.

## Task routing

Read the matching skill when the task needs its workflow. The maintained skills live in `.agents/skills/`, where Codex discovers them automatically in a fresh project session.

| Task | Skill |
| --- | --- |
| Allocation API, identity, lifecycle | [ipam-contract](.agents/skills/ipam-contract/SKILL.md) |
| AWS observation, IAM, reuse evidence | [aws-reconciliation](.agents/skills/aws-reconciliation/SKILL.md) |
| Terraform provider, state, import, replacement | [terraform-provider](.agents/skills/terraform-provider/SKILL.md) |
| Local Docker Compose workflow | [compose-development](.agents/skills/compose-development/SKILL.md) |
| Importing existing accounts, networks and IP ranges; organization inventory | [onboarding-import](.agents/skills/onboarding-import/SKILL.md) |
| Assessing which observed VPC CIDRs conflict, offline, before any import | [overlap assessment](docs/OVERLAP_ASSESSMENT.md) and [ADR 0014](docs/decisions/0014-RESOURCE_AWARE_OVERLAP_ASSESSMENT.md) |
| Reviewing a migration plan and deriving its progress (`onboard progress`, `client evidence`) | [migration progress](docs/MIGRATION_PROGRESS.md) and [ADR 0015](docs/decisions/0015-REVIEWED_MIGRATION_PLAN_AND_DERIVED_PROGRESS.md) |
| Adopting an imported network as an owned allocation | [adoption runbook](deploy/runbooks/ADOPTION.md) and [ADR 0010](docs/decisions/0010-ADOPTING_EXISTING_NETWORKS_AS_ALLOCATIONS.md) |
| Running the stack end to end across REST, CLI, Terraform, and the UI | [end-to-end suite](tests/e2e/README.md) |
| Picking up planned work (UI authentication, organization inventory, onboarding import) | [work plan](docs/WORK_PLAN.md) — take one package, follow its contract |
| Stage/prod Helm validation | [helm-validation](.agents/skills/helm-validation/SKILL.md) |

## Working boundaries

- Keep company allocation policy in platform-ipam; NetBox-specific fields belong in its adapter/configuration.
- Terraform planning and data-source reads never allocate. Preserve stable allocation keys across retries.
- Incomplete AWS observations mean UNKNOWN and cannot establish safe address reuse.
- Development uses Docker Compose; stage/prod use Kubernetes and Helm. Go and Terraform run from pinned images, not from host toolchains.
- Markdown files are named in upper snake case: `docs/API_V1.md`, and `docs/decisions/0006-OPERATOR_UI_AUTHENTICATION.md` for decision records (number, hyphen, then the title). `README.md` and `SKILL.md` keep their conventional names.
- Architecture decisions live in `docs/decisions/`. Record a new one when a decision constrains later work rather than only describing current code.
- Keep proposed contracts, syntax checks, and demonstrated runtime behavior distinct. The repository may not yet contain the implementation a check requires.
- Use AWS Knowledge for AWS documentation, Terraform MCP for registry/provider usage, and Context7 for implementation dependencies. Fetch a precise version/topic before broad searches.
- Prefer `rg`, native CLIs, and the relevant `scripts/ai/check-*` command. These checks return compact JSON; read the referenced log only for a failure or blocker. Exit 2 means incomplete verification, not success.
- Run checks appropriate to the change. Repeat only for new edits, failures, or unresolved concerns. No live AWS tests or deployments are implied by static validation.
- Report the change, validation evidence, and remaining limits. Keep large logs, inventory dumps, secrets, and repetitive source material out of the conversation.

[Agent tooling setup and commands](docs/AI_TOOLING.md) explains the project MCP launcher and current setup limits.
