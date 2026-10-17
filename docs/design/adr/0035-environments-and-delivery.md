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
`signing-key`, `release-roles`, `ecr`, `private-ca`; from T40b `network`,
`cell`, `global` and `subordinate-ca`) and one root per environment, region
and cell under `envs/` (T40b: `dev` and `test`, one cell each).
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
P-384, kept from deletion) signs one subordinate CA per environment (T40b:
EC P-256 in the environment's account, path length 0, general-purpose mode;
the environment's root signs it through a provider for tooling);
cert-manager with the `aws-privateca-issuer` issues the short-lived
certificates of TLS inside a cell and of mTLS between services. A Network
Load Balancer passes TLS through to the router (layer 4), which terminates
it; it therefore has no access logs (they cover TLS listeners only, and
their bucket must use SSE-S3, which the policy refuses) and the router logs
every request. PostgreSQL requires TLS 1.3 (`ssl_min_protocol_version`,
`rds.force_ssl`).

**Encryption at rest, checked.** `deploy/policy/encryption.rego` refuses
any S3 bucket without KMS encryption and a full public access block, any
symmetric KMS key that does not rotate, any ECR repository or replication
template not encrypted with KMS, any RDS instance or cluster, EBS volume,
EKS cluster (secrets) or DynamoDB table not encrypted; from T40b also a
launch template's volumes, a log group or secret without a KMS key, EBS
encryption by default turned off, an EFS file system or ElastiCache
replication group not encrypted. `deploy/policy/endpoints.rego` refuses a
VPC without endpoints for S3, KMS and STS (T38b: providers are never
proxied). conftest applies it
to every module and root in CI, with unit tests, and checks that it refuses
each resource of an unencrypted fixture. It reads the Terraform source, so
it sees resources declared in the directory, not inside a called module:
each module is checked in its own directory. EBS encryption by default is
on in every environment account (T40b), and the rules grow with the
resources T40b brings (launch templates' volumes, log groups, file systems,
caches).

**Cells (T40b, T40c).** EKS; RDS for PostgreSQL 17 as a Multi-AZ cluster
(in the production shape; see Two shapes) with two readable standbys and point-in-time recovery, behind PgBouncer in
transaction mode (RDS Proxy pins sessions on the `SET LOCAL` that
row-level security relies on, ADR-0003); NATS JetStream on EKS with
encrypted volumes; Temporal self-hosted per cell, for isolation and
residency. Secrets come from AWS Secrets Manager through the External
Secrets Operator into Kubernetes secrets mounted as files, which the
platform reads with `file:` references (ADR-0017). Workloads reach AWS with
their service account's role (IRSA, ADR-0024).

T40b builds a cell as one module, described here in the production shape:
a VPC over three zones (public subnets for load balancers, private ones for
nodes and databases, a NAT gateway per zone) with a gateway endpoint for S3 and interface endpoints with private
DNS for KMS and STS, so calls to the providers never leave AWS's network
or pass a proxy, plus ECR, Secrets Manager, CloudWatch Logs, EC2 and ELB.
Private DNS answers for the regional names only: the workloads must use
the regional endpoints the cell outputs (`aws_endpoints`), above all
`ERP_AWS_STS_ENDPOINT`, whose default `https://sts.amazonaws.com` would
leave through the NAT gateway (T40c sets them);
three KMS keys (data, cluster, and fields of ADR-0019, which
administrators and the account manage but can neither use nor grant: only
the server's role encrypts and decrypts with it, and only with a `tenant`
encryption context; anyone else would first have to change the key
policy, which CloudTrail records); EKS with a private API unless operator
CIDRs are given, access entries instead of the creator's implicit
administrator, secrets encrypted, IMDSv2 with a hop limit of one so pods
use IRSA, encrypted gp3 node volumes on Graviton; the RDS cluster
(`db.m6gd.large` by default, at least 400 GiB of gp3 with 12000 IOPS,
since gp3 below 400 GiB takes no provisioned IOPS and a Multi-AZ DB
cluster requires them; to be confirmed for the region at first apply with
`aws rds describe-orderable-db-instance-options`, and lowered if it
allows) with a parameter group setting `rds.force_ssl=1` and
`ssl_min_protocol_version=TLSv1.3`, its master password managed by RDS in
Secrets Manager; the object bucket (ADR-0016); empty Secrets Manager
entries under `erp/<cell>/` that operators fill, so no secret value is in
Terraform state; and IRSA roles for the server, External Secrets (read
only), cert-manager's issuer (the environment's subordinate and
`EndEntity*` templates only, so every Certificate of T40c must declare
`server auth` and/or `client auth`, from which the issuer picks such a
template) and the EBS CSI driver. NATS and Temporal run on the
cluster (T40c) on volumes encrypted with the cluster key; Temporal's
databases live on the cell's database, so T40c sizes PgBouncer's pools
and Temporal's connections within its budget (a `db.t4g.medium` allows
about 400 connections, shared with the platform). The role of the AWS Load
Balancer Controller comes with its chart (T40c).

**Two shapes (product owner, T40b).** Staging and production run the
production shape above (`zone_redundant`, the default). Dev and test turn
it off: the cell's database is a single-AZ RDS for PostgreSQL 17 instance
(`db.t4g.medium`, 20 GiB of gp3, growing to 200) instead of the Multi-AZ
DB cluster, the VPC has one NAT gateway, and the interface endpoints sit
in the first zone only, where the instance is placed too; the directory
runs in that zone as well. Everything
else is the same in both shapes: TLS 1.3 only, encryption with the cell's
keys, two weeks of point-in-time recovery, deletion protection, and S3,
KMS and STS reached only through their endpoints, so both policies apply
unchanged. A zone's outage takes a dev or test cell down; that is the
saving.

**Global (T40b).** Per environment, in its home cell's VPC: the directory
(05 §5.4, ADR-0022) on its own RDS for PostgreSQL 17 instance, Multi-AZ
in the production shape,
TLS 1.3 only, with a key of its own; and the internet-facing Network Load
Balancer, TCP 443 to the router's pods as IP targets (registered by the
`erp-global` chart's TargetGroupBinding, T40c), health-checked over TCP,
protected from deletion. The router sees the load balancer's addresses,
not the clients': passing the client address (proxy protocol v2, which the
router must then read) comes with the router's TLS in T40c. Revocation is
by short lifetime (see Revocation).

**Helm (T40c).** An `erp-cell` chart (the server, PgBouncer, NATS,
Temporal) and an `erp-global` chart (the router); non-root, read-only root
file system, network policies, disruption budgets. Migrations run once per
release and cell: a `pre-install,pre-upgrade` hook Job runs
`erp-server migrate`, the runner's lock serializing anything concurrent.
The Job reads only the database, `ERP_ENV`, the log level and the pool
size, so it holds no key, issuer or store and can run before the
release's other resources exist; its `DATABASE_URL` is the database's
writer itself, not PgBouncer, whose transaction pooling would break the
runner's session lock and its statements outside a transaction;
the server's pods keep `ERP_MIGRATE_ON_START=false` and become ready only on
the schema they were built for. Kyverno admits only images signed with the
release key, requiring a `cosign/sign/v1` signature (cosign alone takes an
attestation for one, T39b) and both attestations.

**Cluster add-ons and policies (T40c2).** Each cell runs five upstream
charts, pinned by version and by their archive's SHA-256 in
`deploy/helm/addons/charts.txt`, each a release of its own in its
namespace: cert-manager, aws-privateca-issuer, the External Secrets
Operator, the AWS Load Balancer Controller (for TargetGroupBinding only:
it registers targets and creates no load balancer, listener or security
group rule) and Kyverno. Each runs as an IRSA role of the cell
(`workload_role_arns`) trusting exactly its service account; the EBS CSI
driver is an EKS add-on with its role. The `erp-cluster` chart, installed
after them, holds the cell's own resources: the `AWSPCAClusterIssuer` on
the environment's subordinate, the `ClusterSecretStore` usable from the
platform's namespaces only, the `erp-encrypted` StorageClass on the
cluster key, and Kyverno policies, the image ones over the governed
namespaces (`erp`, `erp-global`), the certificate ones over all:
- `erp-release-signed` (an ImageValidatingPolicy) admits an image only with
  three in-toto statements signed by the release key: the
  `https://sigstore.dev/cosign/sign/v1` statement that cosign v3 writes for
  a signature, the SPDX SBOM and the SLSA provenance. Requiring the
  signature by its statement type is what refuses an image that carries
  only attestations: a signature check alone accepts them (T39b), which the
  checks show. The key is the release key's public half (envs/tooling
  `cosign_public_key`) in the policy, so verification needs no KMS call;
  Kyverno reads the bundles from tooling's registry as its IRSA role,
  with the same read actions the repositories grant the environment
  accounts (pull, describe and list images: referrers or signature tags).
  An update is verified again only when it changes an image, so a
  registry outage or a key rotation does not block finalizers and labels
  of running Pods; the webhook waits up to 30 seconds.
- `erp-release-images` refuses any image from elsewhere, and any image by
  tag. Upstream images the platform runs in its namespaces (PgBouncer,
  NATS) are therefore copied into the release registry and signed by the
  release pipeline (T40c3). Both image policies have a twin on
  `pods/ephemeralcontainers`, the subresource `kubectl debug` adds
  containers through, since Kyverno generates the controllers' policies
  only for a policy on Pods alone.
- `erp-service-certificates` judges every Certificate on an AWS Private CA
  issuer (any of them runs as the issuer's role, and the issuer is
  cluster-wide) and every one in the governed namespaces:
  only from `erp-subordinate` and only in those namespaces, a lifetime of
  at most 24 hours renewed with at least 8 left, no CA, `server auth`
  and/or `client auth` and no other extended usage (the issuer's role may
  use end-entity templates only), exactly one SPIFFE identity in the
  environment's trust domain, and its own namespace's service names at
  most, without wildcards (no common name, address, e-mail or other
  name).
- `erp-service-certificate-requests`: a CertificateRequest reaches the
  issuer without a Certificate's checks, its names in a CSR no policy
  reads, and cert-manager approves every request. So only cert-manager's
  controller may make one to an AWS Private CA issuer, that one
  `erp-subordinate` (from a Certificate judged above), in the governed
  namespaces, with the same lifetime, CA and usage rules.

**Revocation (product owner, T40c).** By short lifetime, with no CRL or
OCSP: service certificates live 24 hours and are renewed after 16, the
policy above enforcing both; each environment trusts its own subordinate
CA's certificate as the anchor in `ERP_TLS_CA`, not the root, so a
compromised subordinate is distrusted by replacing it and its bundle, and
no environment's certificate is trusted in another. None of the
platform's TLS clients checks revocation (Go's crypto/tls, pgx, the NATS
and Temporal SDKs), so a CRL or OCSP responder would cost money and
change nothing; a CRL bucket would also need an exception to the
encryption policy. ADR-0033 records it with the transport (T40c4).

`scripts/helm-checks.sh` checks all of it without a cluster: the add-ons
pulled and verified against their pins, rendered with an environment's
values and validated with kubeconform, each running as the service account
its role trusts; the erp-cluster chart validated against the pinned
add-ons' CRDs and refusing to render without any value it requires; and
`scripts/admission-checks.sh`, which signs and attests images with cosign
v3 as the release does in a local registry with the referrers API (zot)
and has Kyverno's CLI admit only the fully signed ones, by digest, from
the release registry (and refuse attestations without a signature, a
missing SBOM or provenance, another key, a tag, a foreign registry, an
unsigned init container and an unsigned Deployment; the CLI cannot
model the ephemeral-container subresource, so the twins are checked to
be the same policies), and judge certificates and requests, each case
differing from an admitted one in one respect.

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
   account's apply role, which also needs IAM permissions of its own for
   them (S3 on the bucket; `kms:Encrypt`, `kms:Decrypt`,
   `kms:GenerateDataKey` on the key), as every cross-account grant does.
   The first `init` also shows whether the lock file is written with the
   state key: if the backend asked for SSE-S3 on it, the bucket refuses it
   and locking fails at once, visibly.
4. Apply `envs/tooling`. `admin_role_arns` must include the role that runs
   the apply, by its full ARN (an SSO role is
   `arn:aws:iam::<account>:role/aws-reserved/sso.amazonaws.com/<region>/AWSReservedSSO_<set>_<id>`):
   KMS refuses a key policy that would lock its creator out. The signing
   key has no account-root statement, so IAM alone grants nothing on it;
   keep more than one administrator role, since if every one were deleted
   only AWS Support could restore access. Set the release environment's
   variables from the outputs (`cosign_key`, `release_role_arn`, `region`).
5. Apply each environment root (`envs/dev`, `envs/test`) in its account,
   with `root_ca_arn` and `root_certificate` from tooling's outputs and
   `tooling_role_arn` naming a role in tooling the applying principal may
   assume that may issue from the root CA (`acm-pca:IssueCertificate`,
   `acm-pca:GetCertificate`). `admin_role_arns` must include the applying
   role, as in step 4. The root creates the account's Auto Scaling
   service-linked role, which the cell's volume key names and KMS only
   accepts once it exists; in an account that already has it, import it
   first (`terraform import aws_iam_service_linked_role.autoscaling
   arn:aws:iam::<account>:role/aws-service-role/autoscaling.amazonaws.com/AWSServiceRoleForAutoScaling`).
   The role in tooling is made by hand for now. A second cell in the same
   account and region sets
   `manage_ebs_defaults = false`: EBS encryption by default is a setting of
   the account and region, which only one cell owns.

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
