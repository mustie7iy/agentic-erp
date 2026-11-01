# ADR-0032: Build, release and supply chain

**Status:** accepted (2026-10-16, product owner, T39)

## Context
Releases are container images promoted through dev, staging, a canary cell
and all cells (06 §6.9), but nothing built an image: CI built the CLI only,
the image scan §6.5 lists did not exist, no binary knew its version, and no
deployment could tell an image this repository built from any other. The
three binaries (`erp-server`, `erp-router`, the `erp` CLI) are pure Go
and already build bit for bit the same from the same commit
(`CGO_ENABLED=0`, `-trimpath`, an empty build id).

## Decision

**Images.** One image per binary, built by ko from `.ko.yaml` with no
Dockerfile, on `gcr.io/distroless/static-debian12:nonroot` pinned by the
digest of its multi-platform index: no shell or package manager, uid 65532,
CA certificates and time zones included. Platforms linux/amd64 and
linux/arm64. The weekly run fails when the tag has moved off the pinned
digest, so a new base comes in a reviewed change. ko builds one binary and
platform at a time (`--jobs=1`): three of the binaries compile `api/gen`,
about 3 GB each with an empty cache, and two at once stopped CI's 7938 MiB
runner (2026-10-27).

**Reproducible.** Layers are dated at the epoch and the binaries carry no
path or build id, so an image's digest is a function of the commit and
`.ko.yaml`. CI builds every image twice, each with empty Go and ko caches
of its own, and fails unless the digests match: on a pull request for
linux/amd64 only, which fits the runner's disk, and in the weekly run (and
before a release, by starting it) for both platforms.

**Version in the binary.** `internal/platform/buildinfo` reports the
release version stamped at link time (`ERP_VERSION`, `devel` when unset)
and the commit Go records. The servers log both when they start and serve
them on their admin listener (`GET /version`); the CLI prints them
(`erp version`).

**SBOM and scan.** ko writes an SPDX SBOM per image. grype scans each SBOM
(the Go modules) and the pinned base image (its Debian packages) and fails
on a high or critical vulnerability that has a fix; accepted exceptions go
in `.grype.yaml` with a reason and an expiry, which review enforces (grype
does not). ko's SBOM does not list the Go standard library, where most Go
advisories are: govulncheck covers it, and the modules, in the code that is
called, on every push and again weekly. The weekly run repeats both scans
on `main`.

**Signature.** Images are signed with cosign using a key in AWS KMS
(`cosign sign --key awskms:///<key ARN>`), not keyless signing: keyless
signing would record the repository and workflow identity in Sigstore's
public transparency log, and this repository is private; KMS keeps the
signing key with the deployment's other keys (ADR-0018, ADR-0019). Nothing
is written to a transparency log: `release/signing-config.json` names no
Sigstore service. Every image, each multi-platform index and each
per-platform image, carries a signature and two attestations signed by the
same key: its SPDX SBOM, and SLSA v1 build provenance written by
`scripts/provenance.sh` (the commit, the workflow run, the base image and ko
version). GitHub's own artifact attestations are not used: for a private
repository they need GitHub Enterprise Cloud, and they would sign with
another key than the one deployment trusts. `scripts/sign-images.sh` does
all three. The key is configuration, not code:

| Setting | Where | Value |
|---|---|---|
| `ERP_COSIGN_KEY` | release workflow (signs) | `awskms:///arn:aws:kms:<region>:<account>:key/<id>`, an asymmetric `ECC_NIST_P256` `SIGN_VERIFY` key |
| `ERP_COSIGN_PUBLIC_KEY` | delivery and the clusters' admission policy (verify) | the same KMS reference, or the exported public key |

The release role may only `kms:Sign` and `kms:GetPublicKey` with it; the
delivery role only `kms:GetPublicKey`. The key comes with T40's
infrastructure; until then the signing tests use a cosign key pair made for
the test.

**Checked before deployment.** `scripts/verify-image.sh` accepts an image
only by digest, with a signature, an SBOM attestation and a provenance
attestation by the release key, and provenance naming `ERP_EXPECTED_COMMIT`
(required; `any` skips the check on purpose) from a clean checkout. `cosign
verify` alone would take an attestation for a signature, so it looks for an
image signature (type `cosign/sign/v1`) itself; the clusters' admission
policy (T40) must require that type as well, not only the key. The release
workflow runs it on what it pushed, delivery runs it again before a rollout
(T40), and the clusters admit only images signed with the key. CI's signing
job proves the round trip on every push against a registry made for the
job, with a key pair made for the run: signed images pass, and another key,
another commit, a tag, a missing signature, attestations without a
signature and provenance from uncommitted changes are refused.

**Versions and changelog.** Semantic `vX.Y.Z` tags on `main`, `0.y.z` until
the pilot and `1.0.0` at its go-live. Major: an accepted contract break (a
commit since the last tag added or changed an entry of
`api/breaking-accepted.txt`, even if a later one removed it), from 1.0 on; minor: a
migration, a contract change or any commit that is not a `Fix:`,
`Refactor:`, `Tests:` (or the older `Test:`), `CI:`, `Plan:`, `Tracker:`,
`Process:` or `Release:`; patch otherwise (`scripts/next-version.sh`; the
person releasing may name the version instead). The changelog is generated
from the commit history by git-cliff (`cliff.toml`, `scripts/changelog.sh`),
grouped by kind (added, fixed, changed) and, within a kind, by tracker item;
tests, CI, plans, tracker, process and release commits are left out. Commit
subjects keep the house style (a sentence naming the T-number, with those
prefixes); §6.9 says so rather than Conventional Commits.

**Who releases.** A manual dispatch of `.github/workflows/release.yml` from
a green `main` commit, approved by a second person through the protected
`release` environment (deployments from `main` only, "Prevent self-review"
on), which also holds the signing key reference, the AWS role the workflow
assumes to use it (its trust policy pinned to that environment) and a GitHub
App for the changelog pull request. The workflow builds the images at that
commit with the version stamped in and scans them, then pushes the same
build (reproducible, so the same digests, which it checks) tagged with the
version only, never a moving tag; signs and attests them, verifies what it
pushed, then tags the commit it built and publishes the GitHub Release with
the generated notes, the image digests and the SBOMs. `CHANGELOG.md` gains
the release's entry through a pull request (`Release: changelog for
vX.Y.Z`), since `main` is protected and the tag already names the built
commit; the App opens it because pull requests opened with the workflow's
own token start no CI. A run that failed after the tag is dispatched again
with the same version and goes on from the tag. No release on every merge
until T40's delivery and rollback exist.

The work is split in T39a (this ADR, images, buildinfo, the reproducibility
gate, the scan), T39b (signing, attestations, `verify-image.sh`) and T39c
(versions, changelog, the release workflow).

## Alternatives
- **A Dockerfile with a multi-stage build.** It works, but reproducibility
  then depends on the builder image, BuildKit's timestamps and the cache; ko
  builds Go images reproducibly by design and writes the SBOM.
- **Keyless signing (Sigstore, GitHub's OIDC identity).** No key to keep or
  rotate, but every signature publishes the repository and workflow names in
  a public log.
- **Conventional Commits with release-please.** It reads versions from
  commit types, but the history and every lane's habits use another
  convention, which git-cliff can read as it is.
- **An image scan of the pushed image only.** It finds the same issues one
  step later, after the image exists in a registry.

## Consequences
- CI builds the images on every push (about two minutes, twice for the
  gate) and fails on a reproducibility break or a fixable high
  vulnerability; a vulnerability without a fix is reported, not blocking.
- A new distroless digest, ko or grype version is a reviewed change.
- The KMS key, its policy and the clusters' admission policy are T40's
  infrastructure; T39b's tests sign with a local key.
- Without a transparency log or a timestamp authority a signature carries no
  trusted time: if the KMS key is ever compromised, signatures made before
  cannot be told from those made after, so every image signed with it is
  re-signed with a new key and the old one is no longer trusted.
- Go records the commit of the repository it finds a `.git` directory in; a
  build inside a git worktree (whose `.git` is a file) stamps the enclosing
  repository's commit instead. CI and releases build from a clone.

## Amendment (2026-10-19, T55, ADR-0044)

One scoped cgo exception: `erp-analytics` embeds DuckDB (C++) and is built
with `CGO_ENABLED=1` on `gcr.io/distroless/cc-debian12:nonroot` pinned by
digest. It keeps everything else this ADR requires: `-trimpath`, an empty
build id, the double build with matching digests, an SBOM (which then lists
the DuckDB library and the base image's glibc), the grype scan, the
signature and `verify-image.sh`. The exception covers that binary only:
`erp-server`, `erp-router` and `erp` stay `CGO_ENABLED=0` on
`static-debian12`, and a cgo import in any of them fails the build. The
scope is narrow because the binary is narrow: read-only access to the
bucket's `analytics/` prefix, no database credentials, reachable only from
`erp-server` over mTLS (ADR-0033). Any other cgo binary needs its own ADR.
Built outside ko, which does not cross-compile cgo: a pinned multi-stage
`deploy/images/erp-analytics.Dockerfile` with docker buildx (T55h).
`scripts/images.sh` builds it after ko's images (T55p), one platform at a
time (each compiles `api/gen` in a few GB of memory), each an OCI archive
dated at the epoch without buildx attestations; it joins them into one
OCI index it writes itself, so the index's digest is known before any
push and `crane push --index` must report the same one, and lists the
index and its images in `images.txt` like ko's. syft writes each image's
SBOM (Go modules, DuckDB, the cc base's packages), its first package the
image's digest, and the index gets one naming its images, so
`sign-images.sh` signs and attests them unchanged. `images-reproducible.sh`
gives each of its two builds a docker-container buildx builder of its
own, removed with its cache; `image-scan.sh` scans the SBOMs and the cc
base; `base-image-current.sh` checks the builder and the cc base besides
ko's; the provenance names both. The release pushes, signs and verifies
it with the others, so a cell's `release.json` takes `images.analytics`
from the release's `images.txt` like every other image.
`scripts/test-images.sh` checks these scripts with fake tools.

## Amendment (2026-10-21, T73a, ADR-0052)

A fourth ko image: `erp-web`, the web workspace, built from `./cmd/erp-web`
in `.ko.yaml` like `erp-server`, `erp-router` and `erp` (`CGO_ENABLED=0`,
`-trimpath`, empty build id, distroless static), through the same double
build, SBOM, grype scan, cosign signature and attestations and
`verify-image.sh`; `release.json` names it as `images.web` (an
`erp-web` repository in ECR), and `TestStaticBinaries` holds it to no cgo.
Its static files (the stylesheet and the one vendored script, htmx, pinned
by SHA-256 and checked at startup and in a test) are embedded in the
binary, served under content-hashed names; the image holds nothing else.
It takes no part in the cgo exception.

## Amendment (2026-10-28, T72i): the release signs the upstream images it copies

The upstream images the cells run in the platform's namespaces (PgBouncer,
NATS: `deploy/images/upstream.txt`, ADR-0035) are copied by every release
and signed like the platform's own. Decided by this ADR's signing design;
no new mechanism:

- **Where in the release.** After the pushed images are checked against the
  scanned build, the release logs in to tooling's ECR and runs
  `scripts/mirror-images.sh` with `ERP_RELEASE_REGISTRY` (the registry's
  host, a variable of the `release` environment): each pinned digest is
  copied into `erp-<name>`, tagged with its upstream tag, its SBOM written
  by syft, and appended to `images.txt`, so `sign-images.sh` signs it and
  attests its SBOM and provenance, `verify-image.sh` checks it before the
  release goes on, and the GitHub Release lists it with its SBOM. crane and
  syft are `images.sh`'s versions, installed outside the checkout.
- **Copy provenance.** The image is a copy, not a build, so its SLSA v1
  provenance says so: build type
  `https://github.com/mustie7iy/agentic-erp/mirror-images@v1`, the source,
  tag and digest copied and the reference it was copied to as external
  parameters, the source `oci://<source>:<tag>` by digest as a resolved
  dependency. `mirror-images.sh` writes that part; `sign-images.sh`
  completes it with what the build provenance says of the release (its
  commit as the other resolved dependency, whether the checkout was clean,
  the workflow, the run and the version) and attests it in place of the
  build provenance, type `slsaprovenance1`, as for any image. The statement's
  subject is the image's digest, which is the source's: the copy keeps it.
- **Verification.** `verify-image.sh` tells a copy from a build by the build
  type. A copy's provenance must name the image's own digest. Every release
  pinning the same upstream digest attests the same image again, so a copy
  passes when one of its provenances names the expected commit from a clean
  checkout, the other releases' provenances counting neither way (a failed
  release's would otherwise refuse the digest until its pin changed), while
  a built image, whose digest
  changes with every commit, must still name that commit alone. An image
  whose provenances say both copy and build is refused. So a promotion
  (`verify-image.sh` on each image of `release.json` with its commit)
  accepts a cell's `images.pgbouncer` and `images.nats` only as the copies
  the release at that commit made of its pins.
- **Checked by** `scripts/test-mirror-images.sh` (the deploy checks), against
  a local registry with a key made for the test: the copy signed with its
  copy provenance at the release's commit beside a built image with its
  build provenance, two releases verifying for either commit and not a
  third or one from uncommitted changes, and refusals of a copy naming
  another digest, an image both copy and build, a build attested at two
  commits, and two copies of one digest or an unreadable one. The test's
  crane is the release's (v0.21.7), and the release refuses to sign with
  `PROVENANCE`, the tests' stand-in for `provenance.sh`, set.
  `mirror-images.sh DRY_RUN=true` plans the real list's copies without a
  registry, which the test runs on `deploy/images/upstream.txt`.

The upstream images are not scanned by grype in the release: their pins are
reviewed as code changes (ADR-0035), and whether a release should also fail
on a fixable vulnerability in them is left to the product owner (T72i's
report). The product owner decided it should (2026-10-31): see the
amendment of that date.

## Amendment (2026-10-29, batch 21 review): the release's own images in tooling's ECR, and erp-temporal

Three findings of T72i's review (tracker, `review:batch21`), as built:

- **One release registry.** The release pushed the platform's images to
  `ghcr.io/<owner>/agentic-erp/erp-*`, but a cell's `release.json` takes
  only `<registry>/erp-<name>` (`promotion-checks.sh`) and the clusters'
  `erp-release-signed` admits only the hosts of `release.registries`,
  tooling's ECR (ADR-0035): no release's own image could be pinned or
  admitted. The release now logs in to tooling's ECR before it pushes and
  pushes, signs, attests, verifies and lists every image there, under
  `ERP_RELEASE_REGISTRY` (refused unless it is a host alone), each binary
  into its `erp-<name>` repository of `modules/ecr`, as the upstream copies
  already were. Decided by ADR-0035 (one ECR repository per binary, the
  release role pushes to ECR, the clusters pull from it) and by this
  ADR's rule that what is verified and deployed is the pushed digest:
  ghcr is dropped, not kept beside ECR, since nothing pulls from it and a
  second copy would be signed for nobody; the workflow no longer asks for
  `packages: write`. `scripts/test-images.sh` holds the release to it:
  every image a release builds or copies lands in a repository
  `modules/ecr` creates and every repository gets one, under the names
  `promotion-checks.sh` requires, every image a `release.json` names is
  one of them, and `release.yml` pushes and copies into
  `ERP_RELEASE_REGISTRY` and names no other registry.
- **erp-temporal is built.** `release.json` and erp-cell require
  `images.temporal` from `erp-temporal`, which ADR-0035 (T40c4) defines as
  the platform's own binary (`temporal/cmd/erp-temporal`, Temporal's server
  with the platform's authorization, a Go module of its own) "built,
  signed and attested with the platform's own", but nothing built it and
  ECR had no repository for it. `scripts/images.sh` now builds it with ko
  from `temporal/` (ko finds builds in its working directory's module
  only), with a configuration written from `.ko.yaml`'s base and
  platforms, so the base stays pinned once and the scan, the base check
  and the provenance cover it unchanged: static, `-trimpath`, an empty
  build id, one build at a time, listed after the four and before
  erp-analytics, with ko's SBOMs; no version is stamped, as the module
  has no `buildinfo`. `modules/ecr` gains `erp-temporal`, and changes to
  `temporal/go.mod`, `go.sum` or `cmd/` run the image checks
  (`scripts/ci-changes.sh`). It is not an upstream image, so it is not in
  `deploy/images/upstream.txt`.
- **Checks fail closed.** A check run inside `if` or `!` whose tool fails
  must refuse, never pass: `verify-image.sh` asserts a built image's clean
  checkout positively (`all(sourceModified == false)`), where
  `select(sourceModified != false)` read a jq error as clean; and the same
  class is fixed in `provenance.sh` (a failed `git status` read as a clean
  checkout), `sign-images.sh` (an SBOM it could not read was skipped, which
  could hide a second SBOM of an image), `next-version.sh` (a failed git
  read as no release yet, no commit or no migration) and
  `promotion-checks.sh` (a failed `git show` read as a file never there, so
  a drill's record could be rewritten as new). Each has a test with the
  tool made to fail. Left as they are, since their failure is closed
  already: `mirror-images.sh`'s tag lookup (a failed lookup copies, and the
  copy's digest is checked after), `signing-roundtrip.sh`'s `git status`
  (a failure takes the stricter commit check) and the `rev-parse -q
  --verify` tag tests of `changelog.sh` and the release workflow.

## Amendment (2026-10-31, PO, T72i review): the release scans the upstream images it copies

The product owner decided (2026-10-31) that a release fails on a fixable
high or critical vulnerability in an upstream image it copies, as it does
for the platform's own. No new mechanism: the same scan, threshold and
exceptions.

### As built

- **Where.** `scripts/image-scan.sh` scans each pin of
  `deploy/images/upstream.txt` after the platform's SBOMs and bases:
  `grype registry:<source>:<tag>@<digest> --platform linux/arm64` with the
  same `--fail-on high --only-fixed --config .grype.yaml` and the grype
  `scripts/ci-tools.sh` pins. The release runs `make image-scan` in its
  first image step, "built and scanned before anything is pushed", so a
  vulnerable pin fails the release before any image is pushed and before
  its copy is made, signed, attested or listed in `images.txt`: earlier
  than "before it is signed", which the decision asked for. Scanning in
  `mirror-images.sh`, just before each copy, was the alternative; it was
  not taken because the platform's images would already be pushed under
  the release's version, which the next attempt from the fixing commit
  usually computes again (`next-version.sh` counts from the last git tag,
  which a release writes last), and `modules/ecr`'s immutable tags would
  then refuse that push.
- **One reading of the list.** `scripts/lib/upstream.sh`
  (`upstream_images`, `upstream_platform`) reads the list, refusing a name
  that is not a repository name, a digest that is not sha256 and a list
  with no image, for both `image-scan.sh` and `mirror-images.sh`, so the
  scan covers exactly the pins and the platform the release copies. The
  release gives neither another list.
- **Everywhere `make image-scan` runs.** The scan is one script, so CI's
  image job, `weekly.yml` and `make ci-local` scan the pins too: a pin
  change (`deploy/images/`, an images path of `scripts/ci-changes.sh`, as
  is `scripts/lib/upstream.sh` now) is scanned on its own pull request,
  and a vulnerability published later in a pinned image shows in the
  weekly run before the next release meets it.
- **Exceptions** go in `.grype.yaml` as today, each with the
  vulnerability, the package, a reason and an expiry; an upstream finding
  is fixed first by moving the pin to a fixed upstream release (a reviewed
  change to `upstream.txt`), and only accepted when nothing in the cell's
  use of the image reaches it.
- **Checked by** `scripts/test-images.sh` with a fake grype: every pin,
  PgBouncer's and NATS's, scanned once by digest on `linux/arm64` with the
  threshold and `.grype.yaml`; a fixable high in a pin fails the scan; a
  finding the configuration lists passes (a configuration of the test's
  own, `GRYPE_CONFIG`, so the case does not hang on the real file's
  expiring entries; the fake matches the CVE alone, so this proves the
  configuration is read, not grype's package matching); and `release.yml`
  runs `make image-scan`, without `continue-on-error`, in a step before
  every step that pushes, copies or signs, giving the mirror no other list
  and the scan no other configuration (no workflow sets `GRYPE_CONFIG`).
  `scripts/test-mirror-images.sh` checks the shared reader's refusals
  through `mirror-images.sh`. Not checked by the tests: real grype against
  the real pins, which pulls them; `make image-scan` in `make ci-local` and
  CI's image job do that. Not yet known: whether pulling NATS from
  Docker Hub anonymously on every image job meets its pull limits on
  shared runners; if it does, an authenticated pull is the remedy.
