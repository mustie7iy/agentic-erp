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
registry and the root certificate authority. Argo CD runs in each cell
(T40d, product owner), not in tooling. A cell is
a VPC and an EKS cluster in its environment's account and region; a
dedicated cell may get an account of its own.

**Terraform.** `deploy/terraform/` holds modules (`state`, `accounts`,
`signing-key`, `release-roles`, `ecr`, `private-ca`; from T40b `network`,
`cell`, `global` and `subordinate-ca`) and one root per environment, region
and cell under `envs/` (T40b: `dev` and `test`, one cell each; T40d:
`staging` and `production`, whose home cell `prod1` is the canary). Each
environment root calls `modules/environment` (the subordinate CA, the home
cell and the global tier, wired together) with its account, cell, range
and shape; a further cell is a root of its own calling `modules/cell`.
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
protected from deletion. The target group sends a PROXY protocol v2
header and does not preserve client addresses (T40c5), so every
connection reaches the router from the balancer's subnets
(`router_proxy_sources`) and names its client, which the router reads from
those subnets alone (`ERP_ROUTER_PROXY_FROM`; from anywhere else a header
is not looked for, so a client cannot name another address). Revocation is
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

**The erp-cell chart (T40c3).** In the `erp` namespace, after the add-ons
and `erp-cluster` (which also creates the platform's namespaces, under
Kubernetes' restricted Pod Security profile):
- The server: three replicas spread over the zones, a disruption budget,
  production settings only (TLS 1.3 as its certificate's SPIFFE identity,
  `ERP_TRUST_DOMAIN`, operators' issuers, the field key and bucket through
  the regional endpoints, `ERP_AT_REST`), its secrets as files, its IRSA
  role, no service account token. It reaches NATS and Temporal as its
  certificate's identity (T40c4), and seals workflow payloads with
  `erp/<cell>/temporal-codec-key` before they reach the engine.
- NATS JetStream (T40c4): three nodes over the zones on `erp-encrypted`
  volumes (`ERP_AT_REST` nats=volume), clients and routes over TLS 1.3.
  Clients are authorized by SPIFFE identity (`verify_and_map`, ADR-0033):
  the one user is `spiffe://<td>/<cell>/erp-server`, which may publish
  `erp.<cell>.>` and create, update and read its streams, and nothing
  else; another identity, another authority and a client without a
  certificate are refused. NATS maps an e-mail address or DNS name before
  a URI, so no user is named by either. The nodes route to each other
  with certificates of the same CA, and the network policy lets only them
  reach the cluster port. `scripts/nats-checks.sh` runs the chart's
  configuration on the NATS server go.mod pins (the cells' image is that
  version) and checks each of those grants and refusals.
- Temporal (T40c4, product owner): `erp-temporal`, Temporal's server
  (v1.32.1) with the platform's authorization, in a Go module of its own
  under `temporal/`: the server brings about four hundred modules and
  builds only with OpenTelemetry older than the platform's, so it cannot
  share the main module, and the few lines it shares with the platform
  (the SPIFFE ID rule, `file:` references, verified-TLS URLs) are
  repeated there rather than imported. Its claim mapper gives
  `spiffe://<td>/<cell>/erp-server` the writer role on the cell's one
  namespace (`erp`) and the engine's own identity
  (`spiffe://<td>/<cell>/erp-temporal`, its nodes and system worker) the
  system admin role; any other identity, or none, may call the health
  checks alone (Temporal's default authorizer). Every listener and client
  is TLS 1.3 with a client certificate of the environment's CA required;
  `serve` refuses a configuration that does not require one. Two replicas,
  each running all four services, which find each other through the
  database (512 history shards, fixed once the database exists). Its two
  databases (`temporal`, `temporal_visibility`) are on the cell's writer,
  reached directly over verified TLS (Temporal's persistence keeps
  session state that PgBouncer's transaction mode breaks) as a login of
  its own (`erp/<cell>/temporal-database-password`). Each of the four
  services has pools of its own, so it opens at most (replicas + 1 in a
  rollout) × 4 × (10 + 5) = 180 connections, beside PgBouncer's 40,
  within the writer's budget above.
  An `erp-temporal-schema` hook Job creates the login and the databases
  with the master credentials, which the engine never holds, sets the
  login's password again (so a changed one takes effect with the next
  release) and applies the schema versions the binary embeds. `serve`
  registers the namespace (retention 30 days) as the system worker; a
  later change of the retention applies to a new namespace only, an
  existing one is changed with Temporal's own tools.
  Membership (ringpop) runs over the internode TLS, and the internode
  listeners and the engine's clients admit the engine's own identity
  alone: no authorizer runs there, and every service of the environment
  holds a certificate of the same authority. `serve` refuses a database
  store reached without verified TLS (`verify-full`).
  `erp-temporal check-config` loads a configuration as `serve` does, and
  `helm-checks.sh` runs it on the chart's; `scripts/temporal-checks.sh`
  holds the module to the platform's lint, `-race` tests and 100%
  coverage. Its image is built, signed and attested with the platform's
  own (ADR-0032) once T39's release builds the `temporal/` module.
- The migrations hook Job described above, with the writer's URL and the
  RDS bundle as hooks of lower weight, so a first install has them.
- PgBouncer: two replicas, transaction mode, `max_prepared_statements`
  for pgx, TLS 1.3 to its clients (its certificate) and to the writer
  (verified against RDS's bundle). The cell logs in to it with a static
  password operators set (`erp/<cell>/database-client-password`, letters
  and digits, since it goes into a URL); it logs
  in to the writer with the RDS-managed master credentials, which RDS
  rotates weekly. External Secrets refreshes them every five minutes and
  a sidecar of the same image sends PgBouncer SIGHUP when its
  configuration or its daily-renewed certificate changes, so the rotation
  reaches no other component; PgBouncer replaces its server connections as
  they are released, and new ones opened between a rotation and the
  reload fail. In its connection string a single quote is doubled, in its
  user list a double quote.
- Certificates for the server, PgBouncer, NATS and Temporal as `erp-service-certificates`
  requires (a day, renewed with 8 hours left, one SPIFFE identity, the
  service's own names); the environment's subordinate CA certificate is
  the trust anchor (`ERP_TLS_CA`, the clients of PgBouncer).
- Network policies: default deny; the API and admin listener from the
  router's namespace; the server to PgBouncer, NATS, Temporal's frontend
  and 443 (the VPC's AWS endpoints and the tenants' identity providers);
  NATS's and Temporal's nodes to each other; PgBouncer, Temporal and the
  Jobs to the writer in the VPC; DNS. The VPC CNI is a managed add-on of the cell
  with its network policy agent on, which enforces them; whether it lets
  kubelet's probes through is checked at the first install.
- The cell's id is one rule everywhere: a letter, then lower-case letters
  or digits, at most 20 (the platform's `ERP_CELL`), so Terraform's cell
  name, `ERP_CELL` and the SPIFFE path agree (`dev1`, `test1`).
- Images by digest from the release registry. The upstream PgBouncer and
  NATS images are pinned by their linux/arm64 digests in
  `deploy/images/upstream.txt` and copied into `erp-pgbouncer` and
  `erp-nats` with their SBOM by `scripts/mirror-images.sh`, which the
  release runs before `sign-images.sh`, so they are signed and attested
  like the platform's own. Their provenance says what they are, a copy of
  `source@digest` by the release, not a build from this repository: the
  script writes it, and `sign-images.sh` attests it in place of the build
  provenance once T39's release and T40 meet. The script refuses an index,
  another platform, a source that serves another manifest and a release
  tag that names another image.
- The migrations' time is bounded by the installer's timeout as much as
  by the Job's deadline: Helm's `--timeout`, or Argo CD's sync limit of
  90 minutes, which `helm-checks.sh` holds above the Jobs' deadline
  (`migrations.deadlineSeconds`, an hour).

`scripts/helm-checks.sh` renders it, validates it against the add-ons'
CRDs, refuses a render missing any value it requires or naming an image by
tag, holds its workloads to `deploy/helm/policy/workloads.rego` (non-root,
read-only root file system, no capability, no token, digests, resources,
disruption budgets, default deny), and has Kyverno's CLI admit the whole
chart rendered with images signed as the release signs them.

**The erp-global chart (T40c5).** In the `erp-global` namespace of the
environment's home cell, after the add-ons and `erp-cluster`:
- The router: three replicas over the zones, a disruption budget, the
  restricted profile, no service account token. Its public listener
  terminates TLS with the operators' certificate (a public CA's, loaded by
  operators into `erp/global/router-public-certificate` and
  `router-public-key`, product owner: no ACME) and reads the PROXY header;
  its admin listener (liveness, metrics, the global control plane with
  `erp/global/page-token-key`) and its calls to the cells are TLS 1.3 as
  `spiffe://<td>/global/erp-router`, the identity the cells expect.
- A TargetGroupBinding registers the pods by IP in Terraform's target
  group; it manages no security group rule. The pods carry the
  controller's readiness gate, so a rollout's new pod counts only once the
  load balancer finds it healthy, and a stopping pod keeps serving 40
  seconds (`preStop` sleep, longer than the target group's 30-second
  deregistration) while its deregistration spreads, then drains within
  the 75-second grace period.
- The directory's only login is its RDS-managed master user, whose
  password RDS rotates weekly. External Secrets refreshes it every five
  minutes into a mounted file, and the router reads it again for each new
  connection (`ERP_DIRECTORY_PASSWORD`, an `env:` or `file:` reference;
  `database.Settings.Password`), so a rotation needs no restart. New
  connections opened between a rotation and the refresh (up to five
  minutes and the kubelet's sync) fail, and the next request retries;
  open ones keep working. That window is accepted. The home
  cell's server registers in the directory the same way
  (`directory.endpoint` of erp-cell: `ERP_DIRECTORY_URL`, its in-cluster
  API and control URLs), which is how the router finds it. The global
  module creates the three `erp/global/` entries empty, encrypted with the
  global tier's key, and the home cell's External Secrets role may read
  them and the directory's credentials (`global_secrets`), decrypting
  through Secrets Manager only (`kms:ViaService`).
- Network policies: default deny; the public listener from the load
  balancer's subnets alone; the admin listener from no Pod (operators use
  port-forward); the router to the home cell's server, the directory in
  the VPC, 443 (identity providers) and DNS.
`helm-checks.sh` holds it to the same checks and policies as erp-cell,
checks that the PROXY sources, the public listener's network policy and
the readiness gate agree with the load balancer's subnets and the binding,
and has Kyverno admit the router and its certificate.

**Cluster add-ons and policies (T40c2).** Each cell runs upstream
charts, pinned by version and by their archive's SHA-256 in
`deploy/helm/erp-delivery/addons.txt`, each a release of its own in its
namespace: Argo CD (T40d3), cert-manager, aws-privateca-issuer, the External Secrets
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

**Delivery (T40d).** Each cell's own Argo CD pulls `deploy/`. A release is
promoted by a pull request that pins its image digests per environment:
dev, test, staging, a production canary cell, then all cells (06 §6.9). A
promotion merges only after `verify-image.sh` checks the digests against
the release's commit. Production promotions need a second approver
(T40d4), except a rollback to a known-good release (product owner,
2026-10-19). Every release passes a rollback drill in staging before
production: deploy N, deploy N+1, revert to N, smoke tests and readiness
with N's code on N+1's schema, which expand, migrate, contract (§6.4) makes
safe. Rollback in production is the same revert, per cell.

*Argo CD (T40d3, product owner).* One Argo CD per cell, installed as one
of its add-ons, pulls the cell's pinned release from `deploy/` and
deploys to its own cluster alone: no central Argo CD, no other cluster
registered, so nothing holds access to a cell's cluster API from another
account. It has no accounts, Dex or ingress of its own; operators work
through the cell's Kubernetes API with their own access (`kubectl`,
`argocd --core`). It reads the repository with the cell's read-only deploy
key, `erp/<cell>/argocd-repository`, which the bootstrap loads and the
External Secrets Operator's role cannot read. The `erp-delivery` chart is
the cell's app of apps: its root Application renders the chart with the
cell's own values, `deploy/environments/<env>/<cell>/cell.json` (written
from the cell's Terraform outputs, changed by a pull request of their own;
`promotion-checks.sh` refuses one naming another cell or holding an image,
Argo CD's settings, the migrations deadline or any key but the cell's own
values; the repository and branch are the chart's own, so no cell's values
can point it at a branch the checks never saw), into one Application per add-on of `addons.txt` in its
order (one sync wave each; an Application is healthy once what it deploys
is, so each wave waits for the one before), then `erp-cluster`, then
`erp-cell` and, on the environment's home cell, `erp-global`, these two
with the cell's `release.json` as a values file. Every Application is in
one project whose only destination is the cell's own cluster, syncs
automatically with pruning and self-healing, retries twice, and has no
resources finalizer, so deleting one leaves what it deployed (the NATS
volumes among it). Argo CD fetches an add-on chart by its version; the
archive's SHA-256 is checked by `helm-checks.sh`, not at sync.
Under Argo CD erp-cell's Helm hooks are PreSync hooks run by weight: the
wave -10 secrets (an ExternalSecret is complete once ready) and the RDS
bundle exist before the Jobs of weight 0 start, and Argo CD deletes the
succeeded hooks only once the whole sync has succeeded, so they outlive the
Jobs that mount them. `helm-checks.sh` holds every hook Job to mounting
only hooks of a lower weight deleted at most on success. PreSync hooks run
on every sync, a self-heal's too; the migrations and the engine's schema
Job are idempotent, so such a sync costs their run and changes nothing.
`helm-checks.sh` renders the chart and holds it to the order, the pins,
the destination and the release files. The 90-minute limit also bounds
the root Application's own sync, which on a first bootstrap waits for
every wave, the migrations included; if it runs out, it retries while the
children's syncs go on, and a sync started by hand finishes the
bootstrap.

*Promotions (T40d2).* Each cell's release is a file,
`deploy/environments/<env>/<cell>/release.json`: the release's version and
commit and the image digests (server, router, PgBouncer, NATS,
erp-temporal), each from its own repository in the cell's region's
registry. It is JSON, so it is also the release's values file for the
erp-cell and erp-global charts (`images.*`). A promotion pull request
changes one cell's file and nothing else; `deploy/environments/path.json`
orders the cells (`dev/dev1`, `test/test1`, `staging/stg1`,
`production/prod1`, the canary), and a cell may take only the release the
one before it runs on the base branch, every further production cell what
the canary runs, or a release it ran before (a rollback is a revert). A
release reaches production only once `staging/stg1/drills/<version>.json`
lists the runs of its drill (release, commit, the release rolled back to,
result, the run) and the last for the release's commit passed; the record
is appended to by a pull request of its own and never rewritten, so a
failed run stays on it; a version re-pinned to another commit is drilled
again. Each run added must name a release and commit the drill cell's
`release.json` pinned on the base branch, and a release it ran to roll
back to, so a record names what was drilled. A
promotion changes that one file in the whole repository, and the checks
that judge it are the base branch's: the workflow runs as
`pull_request_target` and reads the pull request's commit as data, so a
promotion cannot also rewrite its own checks. `scripts/promotion-checks.sh`
checks all of it offline, `test-promotion-checks.sh` on a throwaway
repository (the `promotion-self-test` workflow, when the checks change);
the `promotion` workflow runs the checks and then, in the
`delivery` environment as the erp-delivery role, `verify-image.sh` on
every pinned image against the release's commit. Only promotion paths run
it and the deploy workflow skips them, so a promotion costs two short
jobs.

*Approvals (T40d4, product owner).* A promotion of a production cell, a
change to a production cell's `cell.json`, and a
change to `path.json` (which orders every later promotion) merge only once someone other than its author
approved its head commit and is listed in `.github/approvers` as the base
branch has it; no CODEOWNERS. `scripts/promotion-approvals.sh` reads the
pull request and its reviews through GitHub's API with the workflow's
read-only token (`contents: read`, `pull-requests: read`): each
reviewer's latest approval, change request or dismissal stands, a comment
changing nothing; an approval counts only for the commit it was given on,
and a head that moved since the checks ran is refused. It is the
`approvals` job of the `promotion` workflow, run only when
`promotion-checks.sh` says one is needed; since that workflow runs as
`pull_request_target`, which a review does not trigger,
`promotion-review.yml` re-runs that one job for the reviewed head on any
review but a comment, or a dismissal, and judges nothing itself.
`scripts/merge-pr.sh`, which stands in for branch protection, merges a
pull request changing `deploy/environments/` only once every job of the
`promotion` workflow passed or was skipped on its head, and runs
`promotion-approvals.sh` itself once more where an approval is needed, so a
change request or a dismissal after the job ran still counts. The list
holds the product owner alone today, who authors the promotions: until a
second approver is listed, no production promotion can merge. A change to
the list itself needs no approval, only the review any change has
(06 §6.5): with one owner, that is the owner's own. That GitHub reports the
`pull_request_target` jobs on the pull request's head (which
`promotion-review.yml` and `merge-pr.sh` read) is confirmed on the first
promotion pull request; if it does not, both fail closed.
Two exceptions (product owner, 2026-10-19), both still checked and kept
in the history: a production rollback to a known-good release, one the
cell ran before and whose drill's last run for that commit passed, needs
no second approver, so a bad release can be reverted at once
(`promotion-checks.sh` says so in its log and leaves `APPROVAL_OUT`
empty); a rollback to a release without that record still needs one. A
pull request that only appends to a drill's record needs none either: the
record is append-only and each run is checked against the release and
commit the drill cell ran. What a cell ran is read from the base branch's
first-parent history, its merged states, so a commit inside a merged pull
request that pinned and then reverted a release does not count. A run's
result and log address are the drill's own word: anyone who may merge
could append a passed run after a failed one and so make a release
production already ran a known-good rollback target. That residual risk
is limited to releases production ran and stays visible in the record's
history; recording the runs from a workflow would close it.

*The rollback drill (T40d5).* `scripts/rollback-drill.sh` drills the
release the drill cell runs (N+1) against the one it ran before (N) on the
cell's own Argo CD: it pauses the root Application's automated sync (and
the drilled ones'), syncs erp-cell (and erp-global on a home cell) to the
main commit that pinned N, then N+1's, then N's again, and after each
step requires the sync succeeded, the Applications are healthy, the
deployments run that release's images by digest and finished rolling out,
and the smoke tests pass (`SMOKE`, a command). Whatever the outcome it
restores the root's automated sync, whose self-heal brings everything back
to the branch, and appends the run, passed or failed, to the record.
Production's rollback reverts `release.json` alone, rendering the
branch's charts with N's images, while the drill's revert syncs N's
commit; so by default the drill refuses to run when `erp-cell` or
`erp-global` changed since N was pinned. `DRILL_CHARTS=current` (product
owner, 2026-10-19) drills such a release at once: every step syncs the
branch's commit with the step's `release.json` set as the Application's
`valuesObject`, which Argo CD gives precedence over the value files, so N
is deployed on the current charts, then N+1, then N again: the rollback
production does, proven across the chart change. The drill removes
those values itself when it ends, in either mode (a field kubectl
patched is not one the root's server-side apply or self-heal removes),
and fails the run if it cannot.
`test-rollback-drill.sh` runs it against a stand-in kubectl (no cluster;
the `promotion-self-test` workflow). Runbook, in staging's account with
access to `stg1`'s cluster, once N+1 is on `staging/stg1`:
1. `git fetch origin && git checkout origin/main`;
2. `SMOKE=<the smoke tests' command> scripts/rollback-drill.sh <https URL of where the drill's log is kept>`,
   keeping its output there (with `DRILL_CHARTS=current` when it refuses
   because the charts changed);
3. commit the record it wrote, `deploy/environments/staging/stg1/drills/<N+1>.json`,
   on a branch of its own and merge it by pull request; a failed run is
   recorded too, and a later run appends.
If the drill is interrupted before it restores the root, restore it by
hand: `kubectl -n argocd patch application erp-delivery --type merge -p '{"spec":{"syncPolicy":{"automated":{"prune":true,"selfHeal":true}}}}'`.
The first run on AWS staging is a pre-pilot step.

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
5. Apply each environment root (`envs/dev`, `envs/test`, `envs/staging`,
   `envs/production`) in its account,
   with `root_ca_arn` and `root_certificate` from tooling's outputs and
   `tooling_role_arn` naming a role in tooling the applying principal may
   assume that may issue from the root CA (`acm-pca:IssueCertificate`,
   `acm-pca:GetCertificate`). `admin_role_arns` must include the applying
   role, as in step 4. The root creates the account's Auto Scaling
   service-linked role, which the cell's volume key names and KMS only
   accepts once it exists; in an account that already has it, import it
   first (`terraform import module.environment.aws_iam_service_linked_role.autoscaling
   arn:aws:iam::<account>:role/aws-service-role/autoscaling.amazonaws.com/AWSServiceRoleForAutoScaling`).
   The role in tooling is made by hand for now. A second cell in the same
   account and region sets
   `manage_ebs_defaults = false`: EBS encryption by default is a setting of
   the account and region, which only one cell owns.
6. Bootstrap each cell's delivery, in the cell's account with access to
   its cluster (`aws eks update-kubeconfig`):
   - write `deploy/environments/<env>/<cell>/cell.json` from the root's
     outputs (`cell`, `global`; the add-ons' values as
     `deploy/helm/addons/testdata` shows) and merge it by a pull request;
   - create a read-only deploy key of the repository for the cell, store
     its private half in `erp/<cell>/argocd-repository`, and load it:
     `kubectl create namespace argocd`, then
     `kubectl -n argocd create secret generic erp-repository --from-literal=type=git --from-literal=url=<repository> --from-file=sshPrivateKey=<(aws secretsmanager get-secret-value --secret-id erp/<cell>/argocd-repository --query SecretString --output text)`
     and `kubectl -n argocd label secret erp-repository argocd.argoproj.io/secret-type=repository`;
   - seed the monitoring add-on's secrets (ADR-0043): register Grafana as
     a client of the operators' identity provider (redirect
     `<root_url>/login/generic_oauth`) and store its secret in
     `erp/<cell>/grafana-oidc`; store Alertmanager's receivers, a YAML list,
     in `erp/<cell>/alertmanager-receivers`, `- name: erp-alerts` and
     `- name: erp-heartbeat` (which notify no one) until the receivers are
     decided: a block list, each
     entry starting `- ` at column 0, and no receiver named `erp-null`,
     which the routes keep for the always-firing alerts;
   - install Argo CD from its pinned archive, checked against `addons.txt`:
     `helm pull argo-cd --repo https://argoproj.github.io/argo-helm --version <version>`,
     `shasum -a 256` it, then
     `helm install argo-cd argo-cd-<version>.tgz -n argocd -f deploy/helm/addons/values/argo-cd.yaml`;
   - apply the project and the root Application:
     `helm template erp-delivery deploy/helm/erp-delivery -f deploy/environments/<env>/<cell>/cell.json --show-only templates/project.yaml --show-only templates/root.yaml | kubectl apply -f -`.
   Argo CD then installs the add-ons, itself included, and the platform at
   the cell's release. A new deploy key is loaded the same way.

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
  workflow); plans and applies against the accounts run by hand, never
  from a pull request. Releases reach the clusters only through each
  cell's Argo CD pulling from the repository.
- Each cell runs and upgrades an Argo CD of its own: more to run than one
  central instance, in exchange for no cross-account access to any
  cluster's API and a cell that delivers without the others.
- The steps: T40a (this record, accounts, state, the signing key, the
  release and delivery roles, ECR, the root CA, the encryption policy),
  T40b (cell and global Terraform), T40c (Helm, migrations Job, Kyverno,
  cert-manager), T40d (Argo CD, promotions, the rollback drill).
