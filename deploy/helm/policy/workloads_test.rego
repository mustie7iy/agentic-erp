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

# erp-web on 443: STS's endpoint's /28s and the internet alone.
web := json.patch(deployment({}, container), [
	{"op": "replace", "path": "/metadata/name", "value": "erp-web"},
	{"op": "replace", "path": "/spec/selector/matchLabels", "value": {"app.kubernetes.io/name": "erp-web"}},
	{"op": "add", "path": "/spec/template/metadata", "value": {"labels": {"app.kubernetes.io/name": "erp-web"}}},
])

web_pdb := {"kind": "PodDisruptionBudget", "metadata": {"name": "erp-web"}, "spec": {"selector": {"matchLabels": {"app.kubernetes.io/name": "erp-web"}}}}

sts := {"to": [{"ipBlock": {"cidr": "10.16.48.0/28"}}, {"ipBlock": {"cidr": "10.16.48.16/28"}}], "ports": [{"protocol": "TCP", "port": 443}]}

internet := {
	"to": [{"ipBlock": {"cidr": "0.0.0.0/0", "except": ["10.16.0.0/16", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16"]}}],
	"ports": [{"protocol": "TCP", "port": 443}],
}

web_policy(egress) := {
	"kind": "NetworkPolicy",
	"metadata": {"name": "erp-web"},
	"spec": {"podSelector": {"matchLabels": {"app.kubernetes.io/name": "erp-web"}}, "policyTypes": ["Ingress", "Egress"], "egress": egress},
}

web_release(np) := release([web, web_pdb, deny_all, np])

wide_443(np, what) := sprintf("Deployment/erp-web: NetworkPolicy %s admits on 443 %s; it may reach STS's endpoint's subnets and the internet alone", [np, what])

test_erp_web_reaching_sts_and_the_internet_passes if {
	count(deny) == 0 with input as web_release(web_policy([writer, sts, internet]))
}

test_erp_web_reaching_the_vpc_on_443_is_refused if {
	vpc := {"to": [{"ipBlock": {"cidr": "10.16.0.0/16"}}], "ports": [{"protocol": "TCP", "port": 443}]}
	deny[wide_443("erp-web", "10.16.0.0/16, wider than a /28 and with 10.0.0.0/8 not excepted")] with input as web_release(web_policy([sts, vpc, internet]))
}

test_erp_web_reaching_a_private_subnet_on_443_is_refused if {
	subnet := {"to": [{"ipBlock": {"cidr": "10.16.64.0/18"}}], "ports": [{"protocol": "TCP", "port": 443}]}
	deny[wide_443("erp-web", "10.16.64.0/18, wider than a /28 and with 10.0.0.0/8 not excepted")] with input as web_release(web_policy([subnet]))
}

test_erp_web_reaching_everything_on_443_is_refused if {
	open := {"to": [{"ipBlock": {"cidr": "0.0.0.0/0"}}], "ports": [{"protocol": "TCP", "port": 443}]}
	refused := deny with input as web_release(web_policy([open]))
	refused[wide_443("erp-web", "0.0.0.0/0, wider than a /28 and with 10.0.0.0/8 not excepted")]
	refused[wide_443("erp-web", "0.0.0.0/0, wider than a /28 and with 169.254.0.0/16 not excepted")]
}

test_erp_web_reaching_the_internet_with_the_vpc_alone_excepted_is_refused if {
	vpc_only := {"to": [{"ipBlock": {"cidr": "0.0.0.0/0", "except": ["10.16.0.0/16"]}}], "ports": [{"protocol": "TCP", "port": 443}]}
	deny[wide_443("erp-web", "0.0.0.0/0, wider than a /28 and with 10.0.0.0/8 not excepted")] with input as web_release(web_policy([vpc_only]))
}

test_erp_web_reaching_every_peer_on_443_is_refused if {
	every_peer := {"ports": [{"protocol": "TCP", "port": 443}]}
	deny["Deployment/erp-web: NetworkPolicy erp-web admits every address on 443; it may reach STS's endpoint's subnets and the internet alone"] with input as web_release(web_policy([every_peer]))
}

test_erp_web_reaching_the_vpc_on_every_port_or_a_range_is_refused if {
	vpc := {"ipBlock": {"cidr": "10.16.0.0/16"}}
	what := "10.16.0.0/16, wider than a /28 and with 10.0.0.0/8 not excepted"
	deny[wide_443("erp-web", what)] with input as web_release(web_policy([{"to": [vpc]}]))
	deny[wide_443("erp-web", what)] with input as web_release(web_policy([{"to": [vpc], "ports": [{"protocol": "TCP"}]}]))
	deny[wide_443("erp-web", what)] with input as web_release(web_policy([{"to": [vpc], "ports": [{"port": 400, "endPort": 500}]}]))
	deny[wide_443("erp-web", what)] with input as web_release(web_policy([{"to": [vpc], "ports": [{"port": "https"}]}]))
}

test_erp_web_reaching_the_vpc_on_other_ports_is_not_judged if {
	count(deny) == 0 with input as web_release(web_policy([writer, {"to": [{"ipBlock": {"cidr": "10.16.0.0/16"}}], "ports": [{"protocol": "UDP", "port": 443}, {"port": 444, "endPort": 500}]}]))
}

test_erp_web_reaching_pods_on_443_is_refused if {
	pods := {"to": [{"namespaceSelector": {}}], "ports": [{"protocol": "TCP", "port": 443}]}
	deny[wide_443("erp-web", "a pod or namespace selector")] with input as web_release(web_policy([pods]))
}

test_a_policy_of_every_pod_or_by_expressions_is_judged_for_erp_web if {
	vpc := {"to": [{"ipBlock": {"cidr": "10.16.0.0/16"}}], "ports": [{"protocol": "TCP", "port": 443}]}
	what := "10.16.0.0/16, wider than a /28 and with 10.0.0.0/8 not excepted"
	every_pod := object.union(web_policy([vpc]), {"metadata": {"name": "all"}, "spec": {"podSelector": {}}})
	deny[wide_443("all", what)] with input as release([web, web_pdb, deny_all, every_pod])
	exprs := json.patch(web_policy([vpc]), [{"op": "replace", "path": "/spec/podSelector", "value": {"matchExpressions": [{"key": "tier", "operator": "Exists"}]}}])
	deny[wide_443("erp-web", what)] with input as release([web, web_pdb, deny_all, exprs])
}

test_other_workloads_may_reach_the_vpc_on_443 if {
	vpc := {"to": [{"ipBlock": {"cidr": "10.16.0.0/16"}}], "ports": [{"protocol": "TCP", "port": 443}]}
	count(deny) == 0 with input as release([deployment({}, container), pdb, deny_all, object.union(web_policy([vpc]), {"spec": {"podSelector": {"matchLabels": {"app": "erp-server"}}}})])
}

# A null field is an omitted one to Kubernetes, and is judged so.
test_erp_web_null_fields_are_read_as_omitted if {
	vpc := {"ipBlock": {"cidr": "10.16.0.0/16"}}
	what := "10.16.0.0/16, wider than a /28 and with 10.0.0.0/8 not excepted"
	every_address := "Deployment/erp-web: NetworkPolicy erp-web admits every address on 443; it may reach STS's endpoint's subnets and the internet alone"
	deny[every_address] with input as web_release(web_policy([{"to": null, "ports": [{"protocol": "TCP", "port": 443}]}]))
	deny[wide_443("erp-web", what)] with input as web_release(web_policy([{"to": [vpc], "ports": null}]))
	deny[wide_443("erp-web", what)] with input as web_release(web_policy([{"to": [vpc], "ports": [{"protocol": null, "port": 443}]}]))
	deny[wide_443("erp-web", what)] with input as web_release(web_policy([{"to": [vpc], "ports": [{"protocol": "TCP", "port": null}]}]))
	deny[wide_443("erp-web", what)] with input as web_release(web_policy([{"to": [{"ipBlock": {"cidr": "10.16.0.0/16", "except": null}}], "ports": [{"protocol": "TCP", "port": 443}]}]))
}

test_a_null_selector_selects_erp_web if {
	vpc := {"to": [{"ipBlock": {"cidr": "10.16.0.0/16"}}], "ports": [{"protocol": "TCP", "port": 443}]}
	what := "10.16.0.0/16, wider than a /28 and with 10.0.0.0/8 not excepted"
	no_selector := json.patch(web_policy([vpc]), [{"op": "replace", "path": "/spec/podSelector", "value": null}])
	deny[wide_443("erp-web", what)] with input as release([web, web_pdb, deny_all, no_selector])
	no_labels := json.patch(web_policy([vpc]), [{"op": "replace", "path": "/spec/podSelector", "value": {"matchLabels": null}}])
	deny[wide_443("erp-web", what)] with input as release([web, web_pdb, deny_all, no_labels])
}

test_erp_web_reaching_ipv6_on_443_is_refused if {
	v6 := {"to": [{"ipBlock": {"cidr": "::/0"}}], "ports": [{"protocol": "TCP", "port": 443}]}
	deny[wide_443("erp-web", "::/0, an IPv6 block")] with input as web_release(web_policy([sts, v6, internet]))
}
