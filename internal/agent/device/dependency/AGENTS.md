# OCI prefetch and delta updates — invariants

This package pulls the OCI content a device spec needs (OS images, application images,
application artifacts, volumes) and, when a delta artifact is usable, applies the delta
instead of pulling the full image. Read [internal/agent/AGENTS.md](../../AGENTS.md) first;
everything there applies here as well.

Changes in this package have repeatedly gone wrong in the same way: four conceptually
different digests are all Go `string`s, live in the same structs, and get assigned to one
another. The section below is the invariant that must survive every change.

## The four digests — never cross-assign them

| Digest | Provenance | May be used for | Must never be |
|---|---|---|---|
| **RuntimeDigest** — what the runtime reports is actually running | runtime monitors: Podman/CRI inspection, k8s pod status, `podman.ImageDigests()`; surfaces as `ApplicationStatus.ImageDigests` | STATUS only: reporting what runs, and as the lookup key for `lastDelta` | derived from a spec, a prefetch target, or a delta artifact annotation |
| **DesiredDigest** — what the spec says should run | the desired `DeviceSpec` image reference: pinned `@sha256:…`, or a tag resolved against the registry (`prefetchTask.targetDigest` / `resolvedDigest`) | deciding what to pull and whether the pull is complete | written into any status image-digest field |
| **ArtifactSourceDigest** — what a delta is computed *from* | the delta artifact's source annotation (`OCIDeltaTarget.SourceDigest`) | locating the base image **in the destination store** before applying the delta | treated as a claim about what is currently running, or defaulted to the desired digest |
| **ArtifactTargetDigest** — what a delta is computed *to* | the delta artifact's subject/target | validating that the artifact matches the desired target | assumed equal to the source, or used to report status |

Two of these are SPEC data (`DesiredDigest`) and STATUS data (`RuntimeDigest`); the device
API keeps spec and status strictly separate and so must this package. The other two
(`ArtifactSourceDigest`, `ArtifactTargetDigest`) are metadata *about an artifact* and are
claims about neither. Copying a desired digest into a source digest is the original
digest-delta defect, and it is invisible to the compiler — so it has to be visible in
review.

**The status path and the delta-apply path are independent.** The status path turns
runtime-reported digests into `lastDelta` entries. The delta-apply path consumes a desired
target plus an optional hint. Neither reads the other's intermediate state.

## Path separation: hint vs. discovery

- **Hint supplied** → validate that artifact, check its *source* digest is present in the
  **destination** store, apply it, pull the desired reference, verify availability. On any
  failure → **full pull**. Never fall through from the hint path into discovery.
- **No hint** → discovery may enumerate referrers, filter to candidates whose source is
  present in the destination store, and rank them by summed payload blob size.
- Candidate-size ranking exists **only** on the discovery path.

## Storage identity

"Available locally" is never just "the digest exists somewhere". It means: correct user,
correct location, correct OCI type/runtime, correct image-service endpoint. Intermediate
artifacts may live elsewhere, but the **final** image must be in the destination store the
workload will actually read from.

## File layout and boundaries

| File | Owns |
|---|---|
| `dependency.go` | `PrefetchManager`: OCI target collection (`CollectOCITargets`, `BeforeUpdate`), task scheduling and the task cache, per-runtime delta apply and fallback, and `Status()` aggregation for `lastDelta` |
| `pull_config.go` | pull policy and client configuration for a target |
| `docs.go` | `go:generate` directives for the package mocks |
| `mock_dependency.go`, `mock_pull_config.go` | generated mocks — regenerate with `make generate`, never hand-edit |
| `*_test.go` | `delta_prefetch_test.go` and `delta_cri_test.go` encode the hint/discovery and digest separation rules; they are the durable record of the invariants above |

`dependency.go` is already ~2300 lines and is the main reason changes here grow out of
control. Treat it as frozen in size: **new behaviour goes in a new file in this package**,
not at the bottom of `dependency.go`.

## Change rules for this package

- A change that touches more than **two** files here is by definition not a minimal change.
  Stop and ask before proceeding.
- Never delete or shrink a delta test as part of a behaviour change. Those tests are the
  only durable encoding of the hint-vs-discovery and digest-separation rules; the
  [test-deletion guard](../../../../.github/workflows/ai-test-deletion-guard.yml) will
  flag it, and "replaced obsolete tests" is not an accepted justification.
- Do not rename digest-bearing fields as a side effect of a behaviour change. If a name is
  genuinely wrong, rename it in its own commit, once.
- When you add a digest-valued field, local, or parameter, name it after its role
  (`runtime…`, `desired…`, `artifactSource…`, `artifactTarget…`) and state the provenance
  in a comment. An unqualified `digest` is how the four roles get conflated.
- Before changing delta selection or fallback, write down which of the four digests each
  branch reads and writes. If a branch reads a runtime digest to decide what to pull, or
  writes a desired digest into status, it is wrong regardless of what the tests say.
