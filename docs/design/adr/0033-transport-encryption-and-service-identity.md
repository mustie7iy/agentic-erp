# ADR-0033: Transport encryption and service identity

**Status:** accepted (2026-10-16)

## Context
01-requirements asks for encryption in transit (TLS 1.2+) and at rest.
Before T38 only the tenants' identity providers were reached over verified
TLS (ADR-0031). Every other connection was whatever its URL said:
- PostgreSQL took `sslmode` from `DATABASE_URL` and `ERP_DIRECTORY_URL`.
  pgx's default `prefer` falls back to plaintext, and `require` does not
  verify the server.
- NATS, Temporal, the S3, KMS, Vault and STS clients, the cells'
  listeners, the router's forwarding and the global plane's calls to a
  cell's control listener all ran without TLS or accepted `http://`.

Services and agents prove who they are only by bearer tokens (ADR-0010). A
token stolen in transit or from a log works from anywhere.

## Decision
- **TLS in the application, not a service mesh.** Each process opens and
  accepts TLS itself, so it can refuse plaintext when it starts and can
  read the client certificate an agent presents. A mesh can still be added
  underneath; it does not replace these checks.
- **Production refuses plaintext at startup.**
  - `DATABASE_URL` and `ERP_DIRECTORY_URL` must parse to verified TLS on
    every host pgx may try (`sslmode=verify-full`). `prefer`, `allow`,
    `require` and `verify-ca` are refused, and so is a plaintext fallback.
  - Every `ERP_NATS_URL` server must be `tls://`.
  - The workflow engine is dialled with TLS (T38b).
  - Endpoints of S3, KMS, Vault and STS must be `https://`. The one
    exemption is instance metadata at exactly `169.254.169.254` or
    `fd00:ec2::254`, a link-local address that never leaves the host;
    it is reached directly, never through a proxy (T38b).
  - Providers (S3, KMS, STS) are reached directly with the material's
    client, which trusts `ERP_TLS_CA` and offers them no certificate: the
    cell's identity is for the deployment's own services.
  - Listeners serve TLS only, and a cell's API and control URLs in the
    directory must be `https://` (T38c).
- **Versions.** TLS 1.3 between the deployment's own services
  (`tlsconf.Internal`); TLS 1.2 or later to providers' endpoints
  (`tlsconf.External`). PostgreSQL's floor is set on the server
  (`ssl_min_protocol_version`, T40).
- **Where the certificates come from.** The process reads three secret
  references (ADR-0017):
  - `ERP_TLS_CA`, a PEM bundle trusted in addition to the system roots;
  - `ERP_TLS_CERT` and `ERP_TLS_KEY`, the certificate chain and key it
    presents.

  In a cluster, cert-manager (T40) issues short-lived certificates from AWS
  Private CA and mounts them as files. Vault PKI could write the same files.
  `tlsconf.Store` reads them at start and again every minute, and each new
  connection uses what it read last, so a renewed certificate needs no
  restart. A read that fails keeps the material the store had and is
  logged at error level. An expired certificate, or a key that does not
  match its certificate, fails startup.
- **PostgreSQL** is verified by its server certificate only:
  - the CA comes from `sslrootcert` in the URL, or from the system roots;
  - the database login stays a password or IAM token from the secret
    store;
  - there are no client certificates, because RDS has no certificate
    authentication.
- **Service identity.** Each service's certificate carries one SPIFFE URI
  SAN, `spiffe://<ERP_TRUST_DOMAIN>/<cell-id | global>/<service>`. That
  identity authorizes a hop and nothing more:
  - the cell API accepts only the router;
  - `/control/*` accepts only the global plane, and still requires the
    operator's token;
  - NATS and Temporal authorize the cell by it (their servers' configuration, T40).

  The global plane runs in the router, so the router's one identity,
  `spiffe://<domain>/global/erp-router`, is what the cell API and
  `/control` both serve (403 `platform.peer_not_allowed` otherwise). A
  router accepts from each cell exactly the identity of the cell the
  directory names, so a stolen cell certificate impersonates no other
  cell. A router with an identity reaches cells over TLS only and needs
  `ERP_TRUST_DOMAIN`; its public listener has a public CA's certificate
  of its own (`ERP_ROUTER_PUBLIC_CERT`, `ERP_ROUTER_PUBLIC_KEY`). Its TLS
  references are env: or file: only.

  A certificate is never a principal: it carries no tenant and no
  permissions. The token remains the principal (ADR-0010), and row-level
  security is unchanged (ADR-0003).
- **Agent identity over mTLS** (RFC 8705, T38d).
  - The router terminates the public TLS and asks for a client
    certificate; a self-signed one is acceptable. Public traffic reaches
    it through a layer-4 load balancer (T40).
  - The router forwards the certificate's SHA-256 thumbprint to the cell
    in `ERP-Client-Cert-Thumbprint`. A cell trusts that header only from
    the router's SAN over mutual TLS.
  - In production, a cell refuses an agent token whose `cnf.x5t#S256` is
    missing or does not match. The refusal is a 401 and is logged.
  - A tenant's provider declares that it binds tokens
    (`certificateBoundTokens`). In production, agents from a provider that
    does not are refused.
- **Encryption at rest** (T38e).
  - `ERP_AT_REST` states it per store, and production requires it.
  - The cell checks what it can see: the S3 bucket's default encryption
    must be KMS, and every write the cell makes asks for that default's
    algorithm and key. An algorithm without the key would put objects
    under S3's AWS managed key instead of the bucket's. Presigned uploads
    get the bucket's default; a bucket policy refusing any other
    encryption header is T40's.
  - The PostgreSQL and JetStream statements are verified by T40's policy
    tests.
  - `ERP_OBJECT_DIR` is refused in production.
  - The statement is logged at start and exported as a metric.

## Alternatives
- **Service mesh (Istio, Linkerd) for all transport security:** the process
  could not prove at startup that its connections are encrypted, and the
  router would never see the agent's certificate.
- **Certificates as principals** (an agent CA, certificates registered per
  agent): a second identity system beside the providers'. It would carry
  its own issuance, revocation and tenant binding, which ADR-0031 already
  gives the providers.
- **Reading `sslmode` from the URL string:** pgx also reads `PGSSLMODE`
  and adds fallbacks. The check reads the configuration pgx actually
  parses.
- **Resolving the TLS material once at start:** short-lived certificates
  would expire under a running replica.

## Consequences
- Development keeps plaintext, unless `ERP_TLS_*` is set: then NATS is
  reached over TLS whatever its URL's scheme. Production configurations
  need certificates from T40's issuers before a cell starts.
- Tests make their certificates in memory (`tlsconf/tlstest`).
  - PostgreSQL's TLS path is tested through a TLS-terminating proxy in
    front of the plaintext test database (`pgtest.TLS`).
  - NATS's TLS path is tested on a NATS test server with TLS
    (`natstest.TLS`).
- A store that cannot re-read its material keeps serving with the old
  material until that certificate expires. The error log is the alarm.
- Tenants whose providers cannot bind tokens to certificates cannot run
  agents in production.

## Amendment (2026-10-21, T73c): erp-web's identity and the client's address
- erp-web, the web workspace (ADR-0052), presents its own SPIFFE identity,
  `spiffe://<domain>/global/erp-web`, to the router's public listener: a
  client certificate alone (client auth, no DNS name), issued as the
  router's is. No cell accepts it.
- With it erp-web forwards the signed-in person's address in
  `Erp-Client-Address`. The public listener asks for any client
  certificate without verifying it (agents' are self-signed), so the
  router verifies the presented chain itself, against the deployment's
  authority alone and for client authentication, and believes the header
  only when the certificate names erp-web's ID. Any other caller's header
  is dropped and replaced by the connection's own address.
- The router always sends the cell `Erp-Client-Address`, which the cell,
  like `Erp-Client-Cert-Thumbprint`, trusts from the router's SAN over
  mutual TLS only. The audit trail records it (ADR-0011, amended).
