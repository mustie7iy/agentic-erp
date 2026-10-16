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
linux/arm64. The nightly run fails when the tag has moved off the pinned
digest, so a new base comes in a reviewed change.

**Reproducible.** Layers are dated at the epoch and the binaries carry no
path or build id, so an image's digest is a function of the commit and
`.ko.yaml`. CI builds every image twice, each with empty Go and ko caches
of its own, and fails unless the digests match.

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
called, on every push and again nightly. The nightly run repeats both scans
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
attestation by the release key, and, given `ERP_EXPECTED_COMMIT`, provenance
naming that commit. The release workflow runs it on what it pushed, delivery
runs it again before a rollout (T40), and the clusters admit only images
signed with the key. CI's signing job proves the round trip on every push
against a registry made for the job, with a key pair made for the run:
signed images pass, and another key, another commit, a tag or a missing
signature are refused.

**Versions and changelog.** Semantic `vX.Y.Z` tags on `main`, `0.y.z` until
the pilot and `1.0.0` at its go-live. Major: an accepted contract break
(`api/breaking-accepted.txt` grew since the last tag), from 1.0 on; minor: a
migration, a contract change or any commit that is not a `Fix:`,
`Refactor:`, `Tests:` (or the older `Test:`), `CI:`, `Plan:`, `Tracker:` or
`Process:`; patch otherwise. The
changelog is generated from the commit history by git-cliff, grouped by
tracker item and prefix. Commit subjects keep the house style (a sentence
naming the T-number, with those prefixes); §6.9 is corrected to say so
rather than Conventional Commits.

**Who releases.** A manual dispatch of the release workflow from a green
`main` commit, approved by a second person through a protected GitHub
environment. No release on every merge until T40's delivery and rollback
exist.

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
- Go records the commit of the repository it finds a `.git` directory in; a
  build inside a git worktree (whose `.git` is a file) stamps the enclosing
  repository's commit instead. CI and releases build from a clone.
