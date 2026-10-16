# ADR-0035: Environments and delivery on AWS

**Status:** accepted (2026-10-16, product owner, T40)

## Context
The platform runs as cells on AWS (05 §5.4, ADR-0024): each cell a complete
stack serving a set of tenants in one region, a global router in front.
T39 produces signed, reproducible images (ADR-0032) but nothing provisions
the accounts, registries, keys, clusters and databases they run on, and
nothing delivers a release to them. T38 asked for TLS everywhere with a
private CA and encryption at rest checked in the infrastructure code; T39
for the signing key, the release role, ECR copies per region and an
admission policy. This record decides the environments and the delivery
path; T40 builds them in four steps.

## Decision

**Accounts.** AWS Organizations with an `erp` organizational unit holding
a `tooling` account and one account per environment: `dev`, `test`,
`staging`, `production`. Tooling holds what every environment trusts or
pulls: the release signing key, the roles GitHub Actions assumes, the image
registry, the root certificate authority and, from T40d, Argo CD. A cell is
a VPC and an EKS cluster in its environment's account and region; a
dedicated cell may get an account of its own.

**Terraform.** `deploy/terraform/` holds modules (`state`, `accounts`,
`signing-key`, `release-roles`, `ecr`, `private-ca`; from T40b `cell` and
`global`) and one root per environment, region and cell under `envs/`.
State lives in one S3 bucket in tooling, versioned (superseded versions
kept a year), encrypted with a key of its own, which the bucket enforces on
every write, TLS-only, with S3's own locking (`use_lockfile`), one key per
root; other accounts' apply roles are granted by name. Terraform 1.16,
AWS provider 6.x, lock files committed for the roots. Every root names its
account (`allowed_account_ids`), so a wrong profile fails before it plans.

**The signing key and the roles (from T39).** An asymmetric
`ECC_NIST_P256` `SIGN_VERIFY` KMS key in tooling. Its key policy is the
access model: administrators manage it and cannot sign; the `erp-release`
role signs and reads the public key; `erp-delivery` and the clusters only
read the public key. Both roles are assumed with GitHub's OIDC token, each
trusting exactly `repo:mustie7iy/agentic-erp:environment:<release|delivery>`
and the STS audience. The release role also pushes to ECR. The tooling
root's outputs are the release environment's `ERP_COSIGN_KEY`
(`awskms:///<key ARN>`) and `ERP_RELEASE_ROLE_ARN`.

**Registry (from T39).** One ECR repository per binary in tooling's primary
region, replicated by ECR to every region a cell runs in, pulled by the
environment accounts. Release tags (`vX.Y.Z`) are immutable; cosign's
`sha256-*` signature and attestation tags are excluded, since signing again
adds to them. KMS-encrypted (replicas through a creation template), scanned
on push. The lifecycle policy only expires release tags beyond the hundred
kept and never untagged manifests, which may be signatures stored as OCI
referrers.

**Certificates (from T38).** A root CA in AWS Private CA in tooling (EC
P-384, kept from deletion) signs one subordinate CA per environment (T40b);
cert-manager with the `aws-privateca-issuer` issues the short-lived
certificates of TLS inside a cell and of mTLS between services. A Network
Load Balancer passes TLS through to the router (layer 4), which terminates
it. PostgreSQL requires TLS 1.3 (`ssl_min_protocol_version`,
`rds.force_ssl`).

**Encryption at rest, checked.** `deploy/policy/encryption.rego` refuses
any S3 bucket without KMS encryption and a full public access block, any
symmetric KMS key that does not rotate, any ECR repository or replication
template not encrypted with KMS, any RDS instance or cluster, EBS volume,
EKS cluster (secrets) or DynamoDB table not encrypted. conftest applies it
to every module and root in CI, with unit tests, and checks that it refuses
each resource of an unencrypted fixture. It reads the Terraform source, so
it sees resources declared in the directory, not inside a called module:
each module is checked in its own directory. EBS encryption by default is
on in every environment account (T40b), and the rules grow with the
resources T40b brings (launch templates' volumes, log groups, file systems,
caches).

**Cells (T40b, T40c).** EKS; RDS for PostgreSQL 17 as a Multi-AZ cluster
with two readable standbys and point-in-time recovery, behind PgBouncer in
transaction mode (RDS Proxy pins sessions on the `SET LOCAL` that
row-level security relies on, ADR-0003); NATS JetStream on EKS with
encrypted volumes; Temporal self-hosted per cell, for isolation and
residency. Secrets come from AWS Secrets Manager through the External
Secrets Operator into Kubernetes secrets mounted as files, which the
platform reads with `file:` references (ADR-0017). Workloads reach AWS with
their service account's role (IRSA, ADR-0024).

**Helm (T40c).** An `erp-cell` chart (the server, PgBouncer, NATS,
Temporal) and an `erp-global` chart (the router); non-root, read-only root
file system, network policies, disruption budgets. Migrations run once per
release and cell: a `pre-install,pre-upgrade` hook Job runs
`erp-server migrate`, the runner's lock serializing anything concurrent;
the server's pods keep `ERP_MIGRATE_ON_START=false` and become ready only on
the schema they were built for. Kyverno admits only images signed with the
release key, requiring a `cosign/sign/v1` signature (cosign alone takes an
attestation for one, T39b) and both attestations.

**Delivery (T40d).** Argo CD in tooling pulls `deploy/`. A release is
promoted by a pull request that pins its image digests per environment:
dev, test, staging, a production canary cell, then all cells (06 §6.9). A
promotion merges only after `verify-image.sh` checks the digests against
the release's commit. Production promotions need a second approver
(CODEOWNERS). Every release passes a rollback drill in staging before
production: deploy N, deploy N+1, revert to N, smoke tests and readiness
with N's code on N+1's schema, which expand, migrate, contract (§6.4) makes
safe. Rollback in production is the same revert, per cell.

**Bootstrap runbook.** Every root is initialised with
`-backend-config="bucket=<bucket>" -backend-config="region=<region>" -backend-config="kms_key_id=<state key ARN>"`:
the S3 backend with `encrypt = true` and no `kms_key_id` writes SSE-S3,
which the state bucket refuses.
1. In the management account, apply `envs/organization` with a
   `backend_override.tf` declaring `backend "local" {}` (the state bucket
   does not exist yet). It creates the accounts.
2. In the new tooling account (entered through `OrganizationAccountAccessRole`
   or an SSO role), apply `envs/bootstrap` the same way, with
   `state_role_arns` naming the management account's apply role. Delete
   the override and run `terraform init -migrate-state` with the backend
   configuration above; its outputs are the bucket and the state key.
3. Back in `envs/organization`, delete the override and migrate its state
   the same way: the bucket and the state key allow the management
   account's apply role.
4. Apply `envs/tooling`. `admin_role_arns` must include the role that runs
   the apply, by its full ARN (an SSO role is
   `arn:aws:iam::<account>:role/aws-reserved/sso.amazonaws.com/<region>/AWSReservedSSO_<set>_<id>`):
   KMS refuses a key policy that would lock its creator out. The signing
   key has no account-root statement, so IAM alone grants nothing on it;
   keep more than one administrator role, since if every one were deleted
   only AWS Support could restore access. Set the release environment's
   variables from the outputs (`cosign_key`, `release_role_arn`, `region`).

## Alternatives
- **One account for everything, environments by VPC.** Cheaper and simpler,
  but a mistake or a compromised role reaches production; accounts are the
  isolation boundary AWS enforces.
- **A deploy job in GitHub Actions pushing to the clusters.** Needs cluster
  credentials in CI and costs runner minutes per rollout; Argo CD pulls, and
  rollback is a revert.
- **Aurora PostgreSQL or RDS Proxy.** Aurora is a different engine to
  operate and price; RDS Proxy pins every session that sets the tenant.
- **Temporal Cloud.** Less to operate, but outside the cell boundary and
  bound to its own regions.
- **sigstore policy-controller.** Verifies signatures only; Kyverno also
  enforces the other cluster policies.
- **Checkov or tfsec for the encryption checks.** Broad rule sets, but
  their defaults are not this design's rules; a small Rego policy states
  exactly what 05 and T38 require and is unit-tested.

## Consequences
- Five accounts and a Private CA cost money before any tenant does: the
  root CA is about USD 400 a month, each subordinate the same unless it
  issues only short-lived certificates.
- ECR replication copies signatures stored as tags; that it also copies
  signatures stored as OCI referrers is checked on the first release by
  verifying the replicated digest in another region.
- Infrastructure changes are checked without credentials in CI (deploy
  workflow); plans and applies against the accounts run by hand or, from
  T40d, through Argo CD's and Terraform's own pipelines, never from a pull
  request.
- The steps: T40a (this record, accounts, state, the signing key, the
  release and delivery roles, ECR, the root CA, the encryption policy),
  T40b (cell and global Terraform), T40c (Helm, migrations Job, Kyverno,
  cert-manager), T40d (Argo CD, promotions, the rollback drill).
