# What every workload the platform's charts render must be (ADR-0035),
# checked on the rendered manifests with conftest --combine:
#   - Pods as the restricted Pod Security profile asks, and more: no root,
#     no privilege escalation, every capability dropped, a read-only root
#     file system, the runtime's seccomp profile, no host namespaces, no
#     service account token unless asked for;
#   - images by digest;
#   - CPU and memory requested, memory bounded;
#   - every Deployment of more than one replica under a disruption budget;
#   - a default-deny network policy in the release;
#   - each pre-install hook Job held, before the default deny exists, by
#     hook network policies of its own that admit the cluster's DNS.
# Kubernetes' Pod Security admission enforces the profile in the cluster
# too (erp-cluster's namespaces); this catches it before a release.
package workloads

import rego.v1

documents contains doc if {
	some file in input
	some doc in contents(file.contents)
	is_object(doc)
}

contents(c) := c if is_array(c)

contents(c) := [c] if is_object(c)

controllers := {"Deployment", "StatefulSet", "DaemonSet", "Job"}

workloads contains doc if {
	some doc in documents
	doc.kind in controllers
}

pod(doc) := doc.spec.template.spec

name(doc) := sprintf("%s/%s", [doc.kind, doc.metadata.name])

containers(doc) := array.concat(object.get(pod(doc), "containers", []), object.get(pod(doc), "initContainers", []))

deny contains msg if {
	some doc in workloads
	not pod(doc).securityContext.runAsNonRoot == true
	msg := sprintf("%s: the Pod must run as non-root (securityContext.runAsNonRoot)", [name(doc)])
}

deny contains msg if {
	some doc in workloads
	not pod(doc).securityContext.seccompProfile.type == "RuntimeDefault"
	msg := sprintf("%s: the Pod must use the runtime's seccomp profile", [name(doc)])
}

deny contains msg if {
	some doc in workloads
	some field in ["hostNetwork", "hostPID", "hostIPC"]
	object.get(pod(doc), field, false) == true
	msg := sprintf("%s: the Pod must not share the node's namespaces (%s)", [name(doc), field])
}

deny contains msg if {
	some doc in workloads
	not object.get(pod(doc), "automountServiceAccountToken", true) == false
	msg := sprintf("%s: the Pod must not mount a service account token (automountServiceAccountToken: false)", [name(doc)])
}

deny contains msg if {
	some doc in workloads
	some c in containers(doc)
	not c.securityContext.allowPrivilegeEscalation == false
	msg := sprintf("%s: container %s must not escalate privileges", [name(doc), c.name])
}

deny contains msg if {
	some doc in workloads
	some c in containers(doc)
	not c.securityContext.readOnlyRootFilesystem == true
	msg := sprintf("%s: container %s must have a read-only root file system", [name(doc), c.name])
}

deny contains msg if {
	some doc in workloads
	some c in containers(doc)
	not "ALL" in object.get(object.get(c.securityContext, "capabilities", {}), "drop", [])
	msg := sprintf("%s: container %s must drop every capability", [name(doc), c.name])
}

deny contains msg if {
	some doc in workloads
	some c in containers(doc)
	object.get(c.securityContext, "privileged", false) == true
	msg := sprintf("%s: container %s must not be privileged", [name(doc), c.name])
}

deny contains msg if {
	some doc in workloads
	some c in containers(doc)
	not contains(c.image, "@sha256:")
	msg := sprintf("%s: container %s must name its image by digest", [name(doc), c.name])
}

deny contains msg if {
	some doc in workloads
	some c in containers(doc)
	some need in [["requests", "cpu"], ["requests", "memory"], ["limits", "memory"]]
	object.get(object.get(object.get(c, "resources", {}), need[0], {}), need[1], null) == null
	msg := sprintf("%s: container %s must set resources.%s.%s", [name(doc), c.name, need[0], need[1]])
}

deny contains msg if {
	some doc in workloads
	doc.kind == "Deployment"
	object.get(doc.spec, "replicas", 1) > 1
	not budgeted(doc)
	msg := sprintf("%s: a Deployment of more than one replica needs a PodDisruptionBudget selecting it", [name(doc)])
}

budgeted(doc) if {
	some pdb in documents
	pdb.kind == "PodDisruptionBudget"
	pdb.spec.selector.matchLabels == doc.spec.selector.matchLabels
}

deny contains "the release needs a default-deny NetworkPolicy (podSelector {}, Ingress and Egress)" if {
	count(workloads) > 0
	not default_deny
}

default_deny if {
	some np in documents
	np.kind == "NetworkPolicy"
	np.spec.podSelector == {}
	"Ingress" in np.spec.policyTypes
	"Egress" in np.spec.policyTypes
	not np.spec.ingress
}

# A pre-install hook runs after the templates are rendered and before any
# of the release's resources are created, in ascending order of weight
# (Helm's chart hooks; Argo CD runs it as PreSync, the weight as its sync
# wave). On a first install the default deny is not there yet: a hook
# Job's Pods are held only by the pre-install hook NetworkPolicies of a
# lower weight that select them, which must isolate them both ways, take
# nothing in, and admit the cluster's DNS themselves, or the Job cannot
# resolve the database it was made to reach (review of T72h).
annotations(doc) := object.get(doc.metadata, "annotations", {})

hook_events(doc) := {trim_space(e) |
	some e in split(object.get(annotations(doc), "helm.sh/hook", ""), ",")
	trim_space(e) != ""
}

hook_weight(doc) := to_number(object.get(annotations(doc), "helm.sh/hook-weight", "0"))

first_install_jobs contains doc if {
	some doc in workloads
	doc.kind == "Job"
	"pre-install" in hook_events(doc)
}

# A selector by expressions is not judged: the Job counts as not held.
selects(np, job) if {
	not np.spec.podSelector.matchExpressions
	labels := object.get(object.get(job.spec.template, "metadata", {}), "labels", {})
	every k, v in object.get(np.spec.podSelector, "matchLabels", {}) {
		labels[k] == v
	}
}

holding(job) := {np |
	some np in documents
	np.kind == "NetworkPolicy"
	"pre-install" in hook_events(np)
	hook_weight(np) < hook_weight(job)
	selects(np, job)
}

dns_rule(r) if {
	some peer in r.to
	peer.namespaceSelector.matchLabels == {"kubernetes.io/metadata.name": "kube-system"}
	peer.podSelector.matchLabels == {"k8s-app": "kube-dns"}
	count({["UDP", 53], ["TCP", 53]} - {[p.protocol, p.port] | some p in r.ports}) == 0
}

deny contains msg if {
	some job in first_install_jobs
	some direction in ["Ingress", "Egress"]
	not isolated(job, direction)
	msg := sprintf("%s: on a first install no pre-install hook NetworkPolicy of a lower weight isolates its Pods' %s (the default deny is not there yet)", [name(job), lower(direction)])
}

isolated(job, direction) if {
	some np in holding(job)
	direction in np.spec.policyTypes
}

deny contains msg if {
	some job in first_install_jobs
	some np in holding(job)

	# Any ingress field, an empty list too, is refused: strict, as default_deny.
	np.spec.ingress
	msg := sprintf("%s: hook NetworkPolicy %s lets connections in to the Job's Pods", [name(job), np.metadata.name])
}

deny contains msg if {
	some job in first_install_jobs
	not admits_dns(job)
	msg := sprintf("%s: on a first install its hook NetworkPolicies must admit the cluster's DNS (kube-system's kube-dns, UDP and TCP 53): the default deny that does is not there yet", [name(job)])
}

admits_dns(job) if {
	some np in holding(job)
	some r in object.get(np.spec, "egress", [])
	dns_rule(r)
}
