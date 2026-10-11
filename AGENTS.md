# Flight Control – Project context for AI assistants

Flight Control is a service for declarative management of fleets of edge devices and their workloads. This file orients AI tools (Cursor, Claude Code, etc.) to the repo layout, conventions, and where to find detailed guidance.

## Repository layout

| Path | Purpose |
|------|--------|
| **api/** | OpenAPI specs and generated types; versioned APIs (v1alpha1, v1beta1). See [api/AGENTS.md](api/AGENTS.md). |
| **cmd/** | Go entrypoints: `flightctl` (CLI), `flightctl-api`, `flightctl-agent`, `flightctl-worker`, `flightctl-periodic`, imagebuilder, PAM issuer, etc. |
| **internal/** | Service and agent implementation. `internal/agent/` has its own [internal/agent/AGENTS.md](internal/agent/AGENTS.md). |
| **pkg/** | Shared libraries (version, config, etc.). |
| **deploy/** | Deployment: Helm (Kubernetes/OpenShift) and Podman quadlets. See [deploy/AGENTS.md](deploy/AGENTS.md). |
| **test/** | Unit (`internal/`, `api/`), integration (`test/integration/`), e2e (`test/e2e/`). See [test/AGENTS.md](test/AGENTS.md). |
| **docs/** | User and developer documentation. See [docs/AGENTS.md](docs/AGENTS.md). |
| **hack/** | Scripts, Containerfiles for local/dev. |
| **packaging/** | RPM (packaging/rpm), Debian, systemd units, SELinux. |

## Build and development

- **Build:** `make build` (requires Go ≥1.26, podman, and other deps; see [docs/developer/README.md](docs/developer/README.md)).
- **Generate API/client code and mocks:** `make generate`; use the repository's `go:generate` directives, which invoke generators from the pinned Go module files.
- **Proto generation:** `make generate-proto` for `api/grpc/`.
- **Unit tests:** `make unit-test` (requires gotestsum: `go install gotest.tools/gotestsum@latest`). Avoid `make test`; prefer `make unit-test` (and `make integration-test` separately if needed). When verifying changes, first run unit tests on the specific files changed, then run `make unit-test` for the full suite. Two opt-out flags are available for faster local iteration: `RACE=0` disables the race detector and `COVERAGE=0` disables the coverage profile (e.g. `make unit-test RACE=0 COVERAGE=0`). Both default to `1` so CI always runs with race detection and coverage enabled.
- **Integration tests:** `make integration-test` (uses testcontainers for Postgres/Redis/Alertmanager; requires Podman). Key options: `INTEGRATION_PROCS=N` for parallelism, `TEST_DIR=./test/integration/store` for specific suites, `INTEGRATION_GINKGO_FOCUS="pattern"` for specific tests.
- **E2E tests:** `make e2e-test` or `make in-cluster-e2e-test`; see [test/AGENTS.md](test/AGENTS.md).
- **Lint:** `make lint` (do not invoke golangci-lint directly; `make lint` installs and configures it automatically), `make lint-openapi`, `make lint-docs`, `make lint-helm`, `make rpmlint`, `make lint-diagrams`.
- **Lint with auto-fix:** `make lint-fix` runs golangci-lint with `--fix`, writing fixes directly back to the working tree. Auto-fixes formatting (`gofmt`, `gci`), typos (`misspell`), and unnecessary type conversions (`unconvert`). Issues requiring human judgment (`gosec`, `staticcheck`, `govet`, `errcheck`, `depguard`, `unused`, `gocyclo`) are still reported but not auto-fixed.
- **Documentation checks:** `make spellcheck-docs` to check spelling, `make fix-spelling` for interactive fixing.
- **Code formatting:** Format Go imports with `gci write --skip-generated -s standard -s default .` (required for `make lint` to pass). Import order: standard library, then all other imports.
- **Dependency management:** `make tidy` to tidy go.mod files after adding/removing dependencies.
- **Cleanup:** `make clean` (containers and volumes), `make clean-all` (full cleanup including `bin/`).

## Running locally

- **Deploy to kind:** `make deploy` (creates cluster, builds containers, deploys Helm, prepares agent config). Optional: `DB_SIZE=small-1k` or `medium-10k` for larger DB.
- **Quadlets (systemd + Podman):** `make deploy-quadlets`. Certs and client config under `$HOME/.flightctl/`.
- **CLI:** After deploy, `bin/flightctl login <server> --web --certificate-authority ~/.flightctl/certs/ca.crt`, then `bin/flightctl apply -f examples/fleet.yaml`, `bin/flightctl get fleets`, etc.
- **Agent VM (Linux host):** `make agent-vm` / `make agent-vm-console`; see [docs/developer/README.md](docs/developer/README.md).

## Key conventions

- **Go:** Standard Go layout; avoid unnecessary dependencies; prefer existing patterns. Agent code follows strict rules—see [internal/agent/AGENTS.md](internal/agent/AGENTS.md).
- **Testing:** Use table-driven tests. Name test cases with "When ... it should ..." format for clarity.
- **API changes:** Edit OpenAPI YAML and hand-maintained types (e.g. `api/core/v1beta1/types.go`), then `make generate`. Do not edit `*.gen.go` by hand.
- **Documentation:** User docs under `docs/user/`, developer docs under `docs/developer/`. Run `make lint-docs` and `make spellcheck-docs` for user docs.
- **Commits:** All commits must be signed (GPG or SSH). Commit messages must be prefixed with Jira issue key (e.g., `<PROJECT>-<NUMBER>: Description`) or `NO-ISSUE:` for trivial changes.
- **Jira references:** Do not include Jira issue keys or Jira URLs in source code, comments, test names, or user-facing documentation. Track work in commit messages, pull requests, and Jira instead.

## Architecture and package layout

- Keep workflow control flow near its entrypoint. Services own business rules
  and events, stores own persistence and atomicity, and tasks orchestrate
  services and external work.
- Reuse existing mechanisms and preserve established layouts unless migration
  is explicitly in scope.
- Server-side code uses legacy/shared and component-based layouts. See
  [internal/AGENTS.md](internal/AGENTS.md) for runtime ownership, package
  layout, mutation-result, interface, and constructor guidance.

## Scope discipline (required for AI agents)

These rules are mechanical and override any instruction to "be thorough". They exist
because an unbounded assistant session can turn a two-line fix into a multi-thousand-line
diff that nobody can review. Two CI checks enforce the numeric parts of this section:
[`ai-diff-budget.yml`](.github/workflows/ai-diff-budget.yml) and
[`ai-test-deletion-guard.yml`](.github/workflows/ai-test-deletion-guard.yml).

### Diff budget

Measure the cumulative branch diff, not the last edit:

```sh
git diff --stat origin/main...HEAD
git diff --name-status origin/main...HEAD
```

| Threshold | Action |
|---|---|
| **15 files** or **500 changed lines** (warn) | Stop and re-read the task. State, per file, why it is in the diff. Continue only if every file is required by the request. |
| **30 files** or **1000 changed lines** (hard stop) | Do not commit or push. Ask the human whether to split the work. A PR over this budget needs the `override/diff-budget` label with a justification in the PR body. |

Generated files (`*.gen.go`, `*.pb.go`), `go.sum`, and `vendor/` do not count toward the
budget. Everything else does, including tests and docs.

### Pre-push checklist

Run this before every `git push`, not just the first one:

1. `git diff --stat origin/main...HEAD` — compare against the budget above.
2. **Justify every file.** Map each changed file to the specific request that required it.
   "Needed for the refactor" is not a justification; if a file cannot be mapped, revert it.
3. **List every deleted test.** Run
   `git diff origin/main...HEAD -- '*_test.go' | grep '^-func Test'`
   and state, per test, why the behaviour it asserted no longer exists. Never summarise
   deletions as "replaced obsolete tests". A net removal of more than 50 test lines needs
   the `override/test-deletion` label and a per-test explanation in the PR body.
4. **Preserve earlier work on the same branch.** Before rewriting a file an earlier commit
   on this branch touched, diff against that commit and list which behaviours you are
   preserving, changing, or removing.
5. `git status --porcelain` is empty — no half-finished work left in the worktree.

`git diff --check` (whitespace) and a green test suite are not a scope review. Run the
scope review first; a green suite on a diff you cannot explain proves nothing.

### Contract-first workflow

1. **Diagnose read-only.** Requests phrased as "why", "explain", "investigate",
   "is this X?", "should we…?" authorise no file edits — not even small or obviously
   correct ones. Produce the root cause, the smallest fix, the exact files and functions,
   explicit non-goals, and an estimated diffstat. Then stop.
2. **Freeze a scope contract before implementing.** Write down, in one page or less:
   the invariants, the in-scope files, the out-of-scope files, the non-goals, and the diff
   budget. Get it approved. The contract — not the conversation — is the requirement.
3. **Implement only what the contract says.** Touching a file outside the allowlist,
   changing behaviour not in the contract, or deleting a test is a stop-and-ask, not a
   judgement call. Decisions you made yourself are proposals, never approvals; label them
   as such when reporting.
4. **Rewrites and renames are opt-in.** Prefer patching the existing code path. Present a
   rewrite as a proposal with its diffstat estimate first. A repo-wide rename is a
   standalone change: never bundle it with behaviour changes, and never rename the same
   symbol twice in one session.
5. **No API surface changes as a side effect.** Edits to `api/**/openapi.yaml`,
   hand-maintained `api/**/types.go`, or anything that regenerates `*.gen.go` require a
   separate, explicitly approved request.
6. **Review findings are not a work queue for the branch under review.** Triage each
   finding into fix-now / follow-up PR / not a defect, and let the human pick.

### No plan files in the repository

Plans, designs, scratch analyses, and task checklists are working artefacts, not
repository artefacts. Keep them in the conversation or under a git-ignored directory
(for example `.artifacts/`). Never commit a plan or design Markdown file to the repository
root; durable design documentation belongs in `docs/developer/`.

## Before committing

1. **Keep docs up to date** – If you change behavior, APIs, or workflows, update the relevant docs in `docs/user/` or `docs/developer/` and run `make lint-docs` (and `make spellcheck-docs` for user docs).
2. **Add test coverage** – New or changed code should include or extend unit tests (and integration tests where appropriate). Prefer table-driven tests and existing patterns; see [test/AGENTS.md](test/AGENTS.md) and [internal/agent/AGENTS.md](internal/agent/AGENTS.md) for agent code.
3. **Tidy dependencies** – Run `make tidy` after adding/removing dependencies or modifying go.mod files.
4. **Run lint** – Run `make lint` before committing and fix any issues. Use `make lint-fix` to auto-fix formatting, typos, and unnecessary conversions.
5. **Run unit and integration tests** – Before committing, run `make unit-test` and `make integration-test` (integration tests require Podman; they use testcontainers for Postgres/Redis/Alertmanager). Fix any failures before pushing.

## Pointers to area-specific guidance

- **API (OpenAPI, codegen, versioning):** [api/AGENTS.md](api/AGENTS.md)
- **Device agent (reconciliation, lifecycle, testing):** [internal/agent/AGENTS.md](internal/agent/AGENTS.md)
- **OCI prefetch and delta updates (digest invariants, hint vs. discovery):** [internal/agent/device/dependency/AGENTS.md](internal/agent/device/dependency/AGENTS.md)
- **Service layer (mockgen/tracing-wrapper conventions for `internal/service/{resource}` sub-packages):** [internal/service/AGENTS.md](internal/service/AGENTS.md)
- **Documentation (structure, style, lint):** [docs/AGENTS.md](docs/AGENTS.md)
- **Deployment (Helm, quadlets, kind):** [deploy/AGENTS.md](deploy/AGENTS.md)
- **Testing (unit, integration, e2e):** [test/AGENTS.md](test/AGENTS.md)

When making changes in a specific area, prefer the corresponding AGENTS.md and the linked docs over duplicating guidance here. For a human-oriented contribution workflow and PR expectations, see [CONTRIBUTING.md](CONTRIBUTING.md).
