# What every workload the platform's charts render must be (ADR-0035),
# checked on the rendered manifests with conftest --combine:
#   - Pods as the restricted Pod Security profile asks, and more: no root,
#     no privilege escalation, every capability dropped, a read-only root
#     file system, the runtime's seccomp profile, no host namespaces, no
#     service account token unless asked for;
#   - images by digest;
#   - CPU and memory requested, memory bounded;
#   - every Deployment of more than one replica under a disruption budget;
#   - a default-deny network policy in the release.
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
