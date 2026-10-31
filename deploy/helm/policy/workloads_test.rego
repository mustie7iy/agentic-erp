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
	"spec": {"replicas": 2, "selector": {"matchLabels": {"app": "erp-server"}}, "template": {"spec": object.union(
		{
			"automountServiceAccountToken": false,
			"securityContext": {"runAsNonRoot": true, "seccompProfile": {"type": "RuntimeDefault"}},
			"containers": [c],
		},
		spec,
	)}},
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

pod_spec := {
	"automountServiceAccountToken": false,
	"securityContext": {"runAsNonRoot": true, "seccompProfile": {"type": "RuntimeDefault"}},
	"containers": [container],
}

hook(weight) := {"helm.sh/hook": "pre-install,pre-upgrade", "helm.sh/hook-weight": weight}

hook_job := {
	"kind": "Job",
	"metadata": {"name": "erp-migrate", "annotations": hook("0")},
	"spec": {"template": {"metadata": {"labels": {"app": "erp-migrate", "tier": "jobs"}}, "spec": pod_spec}},
}

dns := {
	"to": [{"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "kube-system"}}, "podSelector": {"matchLabels": {"k8s-app": "kube-dns"}}}],
	"ports": [{"protocol": "UDP", "port": 53}, {"protocol": "TCP", "port": 53}],
}

writer := {"to": [{"ipBlock": {"cidr": "10.16.0.0/16"}}], "ports": [{"protocol": "TCP", "port": 5432}]}

hook_policy(patch) := object.union(
	{
		"kind": "NetworkPolicy",
		"metadata": {"name": "erp-migrate", "annotations": hook("-10")},
		"spec": {"podSelector": {"matchLabels": {"app": "erp-migrate"}}, "policyTypes": ["Ingress", "Egress"], "egress": [dns, writer]},
	},
	patch,
)

first_install(np) := release([deployment({}, container), pdb, deny_all, hook_job, np])

lacks_dns := "Job/erp-migrate: on a first install its hook NetworkPolicies must admit the cluster's DNS (kube-system's kube-dns, UDP and TCP 53): the default deny that does is not there yet"

not_isolated(direction) := sprintf("Job/erp-migrate: on a first install no pre-install hook NetworkPolicy of a lower weight isolates its Pods' %s (the default deny is not there yet)", [direction])

test_a_hook_job_held_by_its_own_policy_passes if {
	count(deny) == 0 with input as first_install(hook_policy({}))
}

test_a_hook_policy_without_dns_is_refused if {
	deny[lacks_dns] with input as first_install(hook_policy({"spec": {"egress": [writer]}}))
}

test_dns_over_udp_alone_is_refused if {
	udp := object.union(dns, {"ports": [{"protocol": "UDP", "port": 53}]})
	deny[lacks_dns] with input as first_install(hook_policy({"spec": {"egress": [udp, writer]}}))
}

test_dns_of_another_namespace_is_refused if {
	elsewhere := object.union(dns, {"to": [{"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "erp"}}, "podSelector": {"matchLabels": {"k8s-app": "kube-dns"}}}]})
	deny[lacks_dns] with input as first_install(hook_policy({"spec": {"egress": [elsewhere, writer]}}))
}

test_the_default_deny_does_not_hold_a_hook_job if {
	with_dns := object.union(deny_all, {"spec": {"egress": [dns]}})
	refused := deny with input as release([deployment({}, container), pdb, with_dns, hook_job])
	refused[lacks_dns]
	refused[not_isolated("ingress")]
	refused[not_isolated("egress")]
}

test_a_hook_policy_of_the_jobs_weight_does_not_hold_it if {
	refused := deny with input as first_install(hook_policy({"metadata": {"annotations": hook("0")}}))
	refused[lacks_dns]
	refused[not_isolated("egress")]
}

test_a_policy_that_is_not_a_hook_does_not_hold_it if {
	plain := json.patch(hook_policy({}), [{"op": "remove", "path": "/metadata/annotations"}])
	deny[lacks_dns] with input as first_install(plain)
}

test_a_policy_of_other_pods_does_not_hold_it if {
	deny[lacks_dns] with input as first_install(hook_policy({"spec": {"podSelector": {"matchLabels": {"app": "erp-server"}}}}))
}

test_a_selector_by_expressions_does_not_hold_it if {
	exprs := {"matchExpressions": [{"key": "app", "operator": "In", "values": ["erp-migrate"]}]}
	deny[lacks_dns] with input as first_install(hook_policy({"spec": {"podSelector": exprs}}))
}

test_a_hook_policy_isolating_egress_alone_is_refused if {
	deny[not_isolated("ingress")] with input as first_install(hook_policy({"spec": {"policyTypes": ["Egress"]}}))
}

test_a_hook_policy_taking_connections_is_refused if {
	deny["Job/erp-migrate: hook NetworkPolicy erp-migrate lets connections in to the Job's Pods"] with input as first_install(hook_policy({"spec": {"ingress": [{"from": [{"podSelector": {}}]}]}}))
}

test_a_job_that_is_not_a_pre_install_hook_is_not_judged if {
	plain := object.union(hook_job, {"metadata": {"annotations": {"helm.sh/hook": "post-install"}}})
	count(deny) == 0 with input as release([deployment({}, container), pdb, deny_all, plain])
}
