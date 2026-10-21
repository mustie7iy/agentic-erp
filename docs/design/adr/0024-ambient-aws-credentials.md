# ADR-0024: Ambient AWS credentials for the platform's own clients

**Status:** accepted (product owner, 2026-10-21: implemented by internal/platform/awscreds)

## Context
The S3 store (ADR-0016) and the KMS master (ADR-0019) speak their services'
protocols directly, signed with the platform's own Signature Version 4
signer, and took static access keys from settings. Static keys are
long-lived secrets to distribute and rotate; the deployments the platform
targets hand out short-lived role credentials instead: a Kubernetes service
account token exchanged at STS (IRSA), or the instance metadata service of
the machine or task.

## Decision

**A small credentials package** (`internal/platform/awscreds`) resolves
credentials for the signer without the AWS SDK: static keys from settings,
the `AWS_*` environment, a web identity (`AWS_WEB_IDENTITY_TOKEN_FILE` and
`AWS_ROLE_ARN`, exchanged with `AssumeRoleWithWebIdentity`), and the
instance metadata service (version 2, a session token first). The chain
takes the first that has credentials; temporary credentials are cached and
renewed five minutes before they expire.

**Access keys in settings become optional.** `ERP_OBJECT_S3_ACCESS_KEY` and
`ERP_FIELD_KMS_ACCESS_KEY` with their secrets still work and come first;
unset, the ambient chain applies. Half a pair is a configuration error.

**Session tokens are signed.** The signer sends and signs
`X-Amz-Security-Token` for header authentication and puts it in the query
of a presigned URL, as the services require for role credentials.

**Endpoints are settings for tests and emulators.** `ERP_AWS_STS_ENDPOINT`
and `ERP_AWS_IMDS_ENDPOINT` default to the real services. In production (amended
2026-10-16, ADR-0033) STS must be https and the metadata endpoint exactly
`http://169.254.169.254` or `http://[fd00:ec2::254]`, reached directly,
never through a proxy.

## Alternatives
- **The AWS SDK's credential chain.** It would bring the SDK for its
  smallest part; the chain is four providers and one cache.
- **Vault's AWS secrets engine** (ADR-0017 references). Still a credential
  to fetch and rotate; roles bound to the workload need none.
- **Static keys only.** The status quo: a secret per deployment, rotated
  by hand.

## Consequences
- No AWS secret in settings for a deployment on roles; the KMS and S3
  policies bind to the workload's role.
- A credentials failure surfaces on the first signed call, as a store or
  master error, and is retried on the next.
- GCP and Azure equivalents (workload identity, managed identity) belong
  with their stores and masters when those are built.
