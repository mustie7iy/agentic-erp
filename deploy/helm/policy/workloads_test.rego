# Unit tests of workloads.rego (conftest verify).
package workloads

import rego.v1

container := {
	"name": "c",
	"image": "registry.example/erp-server@sha256:1111111111111111111111111111111111111111111111111111111111111111",
	"securityContext": {"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": {"drop": ["ALL"]}},
	"resources": {"requests": {"cpu": "100m", "memory": "64Mi"}, "limits": {"memory": "128Mi"}},
}

deployment(spec, c) := {
	"kind": "Deployment",
	"metadata": {"name": "erp-server"},
	"spec": {"replicas": 2, "selector": {"matchLabels": {"app": "erp-server"}}, "template": {"spec": object.union({
		"automountServiceAccountToken": false,
		"securityContext": {"runAsNonRoot": true, "seccompProfile": {"type": "RuntimeDefault"}},
		"containers": [c],
	}, spec)}},
}

pdb := {"kind": "PodDisruptionBudget", "metadata": {"name": "erp-server"}, "spec": {"selector": {"matchLabels": {"app": "erp-server"}}}}

deny_all := {"kind": "NetworkPolicy", "metadata": {"name": "deny"}, "spec": {"podSelector": {}, "policyTypes": ["Ingress", "Egress"]}}

release(docs) := [{"path": "release.yaml", "contents": docs}]

test_a_compliant_release_passes if {
	count(deny) == 0 with input as release([deployment({}, container), pdb, deny_all])
}

test_root_is_refused if {
	deny["Deployment/erp-server: the Pod must run as non-root (securityContext.runAsNonRoot)"] with input as release([deployment({"securityContext": {"runAsNonRoot": false}}, container), pdb, deny_all])
}

test_host_namespaces_are_refused if {
	deny["Deployment/erp-server: the Pod must not share the node's namespaces (hostNetwork)"] with input as release([deployment({"hostNetwork": true}, container), pdb, deny_all])
}

test_a_service_account_token_is_refused if {
	deny["Deployment/erp-server: the Pod must not mount a service account token (automountServiceAccountToken: false)"] with input as release([deployment({"automountServiceAccountToken": true}, container), pdb, deny_all])
}

test_a_writable_escalating_capable_container_is_refused if {
	c := object.union(container, {"securityContext": {"allowPrivilegeEscalation": true, "readOnlyRootFilesystem": false, "capabilities": {"drop": []}}})
	refused := deny with input as release([deployment({}, c), pdb, deny_all])
	refused["Deployment/erp-server: container c must not escalate privileges"]
	refused["Deployment/erp-server: container c must have a read-only root file system"]
	refused["Deployment/erp-server: container c must drop every capability"]
}

test_an_image_by_tag_is_refused if {
	deny["Deployment/erp-server: container c must name its image by digest"] with input as release([deployment({}, object.union(container, {"image": "registry.example/erp-server:v1"})), pdb, deny_all])
}

test_unbounded_memory_is_refused if {
	deny["Deployment/erp-server: container c must set resources.limits.memory"] with input as release([deployment({}, json.patch(container, [{"op": "remove", "path": "/resources/limits"}])), pdb, deny_all])
}

test_an_init_container_is_judged_too if {
	init := object.union(container, {"name": "init", "image": "registry.example/x:latest"})
	deny["Deployment/erp-server: container init must name its image by digest"] with input as release([deployment({"initContainers": [init]}, container), pdb, deny_all])
}

test_a_deployment_without_a_budget_is_refused if {
	deny["Deployment/erp-server: a Deployment of more than one replica needs a PodDisruptionBudget selecting it"] with input as release([deployment({}, container), deny_all])
}

test_a_release_without_default_deny_is_refused if {
	deny["the release needs a default-deny NetworkPolicy (podSelector {}, Ingress and Egress)"] with input as release([deployment({}, container), pdb])
}
