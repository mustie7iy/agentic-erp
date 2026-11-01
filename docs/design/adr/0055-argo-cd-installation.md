# ADR-0055: Argo CD installed by each cell's bootstrap, not by Terraform or as an EKS managed capability

**Status:** accepted (decided by agents under ADR-0053 d; the product owner
handed the choice to agents on 2026-10-31)

## Context

ADR-0035 (T40d3, product owner) runs one Argo CD per cell, deploying to its
own cluster alone, with no accounts, Dex or ingress of its own: operators
work through the cell's Kubernetes API with their own access. Each cell's
bootstrap (ADR-0035, runbook step 6) loads the read-only deploy key from
`erp/<cell>/argocd-repository`, installs Argo CD once from its pinned chart
archive (version and SHA-256 in `deploy/helm/erp-delivery/addons.txt`,
checked by `helm-checks.sh`) and applies the project and root
Application; from then on Argo CD installs and upgrades the add-ons,
itself included, and the platform at the cell's `release.json`. Its
settings
(`deploy/helm/addons/values/argo-cd.yaml`) set a 90-minute sync limit
(`controller.sync.timeout.seconds: "5400"`) that bounds erp-cell's
migrations Jobs, a custom health check for Applications that orders the
waves, and no notifications.

The Phase 1 triage (product owner 2026-10-21) described T72j as "Argo CD in
Terraform". T72j built the pilot light with Terraform defining everything
Argo CD needs on the DR cluster (the deploy key's replica, the roles, the
cluster) and Argo CD itself bootstrapped there by the runbook, and gave
its reasons (ADR-0040 as built 2026-10-29). Its review raised whether
Argo CD should instead be installed by Terraform, through the Helm
provider or as the EKS managed Argo CD capability; on 2026-10-31 the
product owner answered that agents decide.

## Options

**A. Terraform's Helm provider** (a `helm_release` of Argo CD in the cell
root).
- Terraform would have to reach each cluster's Kubernetes API. The API
  endpoint is private unless `public_api_cidrs` is set
  (`modules/cell/eks.tf`), and plans and applies run by hand from
  operators' machines (ADR-0035), so every apply of a cell root would need
  a route to the private endpoint, or the endpoint opened to it.
- The deploy key would pass through Terraform to reach the cluster, and
  Terraform keeps what it applies in state; today Terraform creates the
  secret empty and its value is set outside it (`modules/cell/data.tf`), so
  state holds no secret value.
- Argo CD manages its own release after the bootstrap (`addons.txt` lists
  it among the add-ons it syncs), so a Terraform-owned release would be
  owned twice: an upgrade by a promotion is drift to the next plan, and a
  plan's revert is drift to Argo CD's self-heal.
- On failover the DR cluster has no nodes until the runbook scales them up,
  and the roots cannot plan while the home region is down (ADR-0040 as
  built), so the DR install would still be a runbook step.

**B. The EKS managed capability for Argo CD.** AWS documents an "EKS
Capability for Argo CD" that runs Argo CD in the AWS control plane, off the
cluster's nodes, with AWS managing its patching and updates. What its
documentation says, and what it means here:
- It requires AWS Identity Center for Argo CD's authentication; local
  users are not supported ([Create an Argo CD
  capability](https://docs.aws.amazon.com/eks/latest/userguide/create-argocd-capability.html),
  Prerequisites). ADR-0035's Argo CD deliberately has no accounts and no
  UI anyone signs in to; operators may reach the AWS accounts through
  Identity Center roles (ADR-0035 runbook), but sign-in accounts on Argo CD
  itself, and Argo CD depending on Identity Center, are a change of
  security posture, for the product owner.
- It does not allow modifying the sync timeout ("fixed at 120 seconds")
  and accepts only some `argocd-cm` fields and three `argocd-cmd-params-cm`
  keys ([Comparing EKS Capability for Argo CD to self-managed Argo
  CD](https://docs.aws.amazon.com/eks/latest/userguide/argocd-comparison.html),
  Unsupported features). Which timeout the 120 seconds is could not be
  confirmed from that page; either way the 90-minute sync limit the
  migrations depend on cannot be set as it is today.
- AWS updates the software ([EKS Capabilities
  considerations](https://docs.aws.amazon.com/eks/latest/userguide/capabilities-considerations.html),
  Operations and maintenance), where ADR-0032 and ADR-0035 make every
  version a reviewed change pinned by digest or checksum.
- It deploys only to EKS clusters named by ARN, through access entries,
  including fully private clusters ([Argo CD
  considerations](https://docs.aws.amazon.com/eks/latest/userguide/argocd-considerations.html));
  it costs an hourly charge per capability (EKS Capabilities
  considerations, Cost Considerations; the amount is not taken from AWS's
  pricing page here). Running off the nodes, it could be created on the DR
  cluster before a failover, which the self-managed Argo CD cannot.
- Whether the hashicorp/aws provider has a resource for it was not
  confirmed from a primary source; nothing here relies on one.

**C. The current approach.** Argo CD installed once per cell by the
bootstrap runbook from its pinned, checksummed archive, then managing
itself as an add-on; Terraform defines everything it needs (the cluster,
the deploy key's empty secret and its DR replica, the roles) and nothing
inside the cluster.

## Decision

C: keep the current approach. Terraform installs nothing inside a cluster:
it gets no access to a cluster's API and its state holds no secret value,
and Argo CD's version stays one pin in `addons.txt`, reviewed and
checksummed like every add-on, with one owner after the bootstrap (Argo CD
itself). A cannot avoid a runbook step on failover and owns the release
twice. B would change the security posture (sign-in accounts on Argo CD
through Identity Center), cannot keep
the migrations' 90-minute sync limit as built, and takes version control
away from review. "Argo CD in Terraform" in the triage's description of
T72j is met as T72j built it: Terraform defines what Argo CD needs, and
the runbook installs it.

Revisit B if its documentation comes to allow the sync limit erp-cell's
migrations need and no sign-in accounts on Argo CD, or if
a cell's failover time (ADR-0040's targets) needs Argo CD running on the
DR cluster before the incident.

## Consequences

- No build: the bootstrap runbook (ADR-0035 step 6), its DR form (ADR-0040
  runbook step 6) and the staging confirmation of Argo CD bootstrapped on
  the DR cluster (tracker, split:T72j) stand as they are.
- Each cell keeps running and upgrading its own Argo CD (ADR-0035
  Consequences), on its own nodes.
- Facts above about the EKS capability are from AWS's EKS user guide as
  read on 2026-10-31; a revisit reads them again.
