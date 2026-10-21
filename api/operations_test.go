package api

import (
	"strings"
	"testing"
)

func TestOperationsFromTheContract(t *testing.T) {
	ops, err := Operations()
	if err != nil {
		t.Fatal(err)
	}
	cancel, ok := ops["cancelOperation"]
	if !ok {
		t.Fatal("cancelOperation: got none, want the operation")
	}
	if cancel.Method != "POST" {
		t.Errorf("cancelOperation method: got %s, want POST", cancel.Method)
	}
	if cancel.Path != "/v1/operations/{id}:cancel" {
		t.Errorf("cancelOperation path: got %s, want /v1/operations/{id}:cancel", cancel.Path)
	}
	if cancel.Permission != "platform.operation.cancel" {
		t.Errorf("cancelOperation permission: got %s, want platform.operation.cancel", cancel.Permission)
	}
	if !cancel.AgentAllowed {
		t.Error("cancelOperation agent allowed: got false, want true")
	}
	if approve := ops["approveApprovalRequest"]; approve.AgentAllowed {
		t.Fatal("approval decisions must be refused to agents")
	}
	// Only proposing writes are x-autonomy: propose; only a POST that
	// changes no business data is x-autonomy: read (ADR-0036).
	for id, want := range map[string]bool{"createRecommendation": true, "createChangeRequest": true, "parkJournalEntry": true,
		"submitRequisition": true, "cancelRequisition": true, "createPurchaseOrder": true, "updatePurchaseOrder": true,
		"createSupplierInvoice": true, "updateSupplierInvoice": true, "cancelApprovalRequest": true,
		"createDownPaymentRequest": true, "cancelDownPaymentRequest": true, "clearSupplierInvoiceDownPayments": false,
		"postSupplierInvoice": false, "releasePurchaseOrder": false, "cancelPurchaseOrder": false, "proposePaymentRun": false, "verifyAuditChain": false} {
		if got := ops[id].Proposes; got != want {
			t.Errorf("%s proposes: got %v, want %v", id, got, want)
		}
	}
	for id, op := range ops {
		if op.Proposes && op.Method == "GET" {
			t.Errorf("%s: a read marked x-autonomy: propose", id)
		}
		if op.Reads != (id == "verifyAuditChain" || id == "previewRevaluation") {
			t.Errorf("%s reads: got %v", id, op.Reads)
		}
	}
}

func TestOperationsRejectsBadContracts(t *testing.T) {
	for name, src := range map[string]string{
		"not yaml":      "paths: [",
		"no permission": "paths:\n  /v1/x:\n    get:\n      operationId: getX\n",
		"bad operation": "paths:\n  /v1/x:\n    get: [1]\n",
		"bad autonomy":  "paths:\n  /v1/x:\n    post:\n      operationId: x\n      x-permission: a.b.c\n      x-autonomy: decide\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseOperations([]byte(src), true); err == nil {
				t.Error("got no error, want the contract refused")
			}
		})
	}
	// A contract without permissions (the control plane) still needs operationIds.
	if ops, err := parseOperations([]byte("paths:\n  /v1/x:\n    get:\n      operationId: getX\n"), false); err != nil {
		t.Fatalf("contract without permissions: %v", err)
	} else if len(ops) != 1 {
		t.Fatalf("contract without permissions: got %d operations %v, want 1", len(ops), ops)
	}
	if _, err := parseOperations([]byte("paths:\n  /v1/x:\n    get:\n      summary: x\n"), false); err == nil {
		t.Fatal("operation without operationId accepted")
	}
	ops, err := parseOperations([]byte("paths:\n  /v1/x:\n    parameters: []\n    get:\n      operationId: getX\n      x-permission: a.b.c\n      x-agent: false\n"), true)
	if err != nil {
		t.Fatalf("path-level parameters: %v", err)
	}
	if len(ops) != 1 {
		t.Fatalf("path-level parameters: got %d operations %v, want 1", len(ops), ops)
	}
	if ops["getX"].AgentAllowed {
		t.Error("x-agent: false: got agent allowed, want refused")
	}
	if !strings.HasPrefix(ops["getX"].Path, "/v1/") {
		t.Errorf("path: got %s, want it under /v1/", ops["getX"].Path)
	}
}

func TestMustOperations(t *testing.T) {
	if len(MustOperations()) == 0 {
		t.Fatal("no operations")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("invalid contract did not panic")
		}
	}()
	mustParse([]byte("paths: ["))
}

func TestSpec(t *testing.T) {
	if !strings.Contains(string(Spec()), "\nopenapi: \"3.1") {
		t.Fatal("spec")
	}
}

func TestDirectoryOperations(t *testing.T) {
	ops, err := DirectoryOperations()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"listDirectoryTenants": "GET /directory/v1/tenants", "createDirectoryTenant": "POST /directory/v1/tenants", "getDirectoryTenant": "GET /directory/v1/tenants/{id}",
		"freezeTenant": "POST /directory/v1/tenants/{id}:freeze", "unfreezeTenant": "POST /directory/v1/tenants/{id}:unfreeze", "moveTenant": "POST /directory/v1/tenants/{id}:move",
		"verifyTenant": "POST /directory/v1/tenants/{id}:verify", "listCells": "GET /directory/v1/cells"}
	if len(ops) != len(want) {
		t.Fatalf("operations: got %d, want %d", len(ops), len(want))
	}
	for id, route := range want {
		t.Run(id, func(t *testing.T) {
			op := ops[id]
			if got := op.Method + " " + op.Path; got != route {
				t.Errorf("route: got %s, want %s", got, route)
			}
			if op.Permission != "" {
				t.Errorf("permission: got %s, want none", op.Permission)
			}
			if !op.AgentAllowed {
				t.Error("agent allowed: got false, want true")
			}
		})
	}
	if !strings.Contains(string(DirectorySpec()), "\nopenapi: \"3.1") {
		t.Fatal("spec")
	}
}

func TestControlOperations(t *testing.T) {
	ops, err := ControlOperations()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"listTenants": "GET /control/v1/tenants", "createTenant": "POST /control/v1/tenants", "getTenant": "GET /control/v1/tenants/{id}",
		"suspendTenant": "POST /control/v1/tenants/{id}:suspend", "resumeTenant": "POST /control/v1/tenants/{id}:resume", "offboardTenant": "POST /control/v1/tenants/{id}:offboard",
		"rotateTenantKey": "POST /control/v1/tenants/{id}:rotateKey", "eraseTenant": "POST /control/v1/tenants/{id}:erase", "getTenantFingerprint": "GET /control/v1/tenants/{id}/fingerprint",
		"listIdentityProviders": "GET /control/v1/tenants/{id}/identity-providers", "registerIdentityProvider": "POST /control/v1/tenants/{id}/identity-providers",
		"getIdentityProvider": "GET /control/v1/tenants/{id}/identity-providers/{provider_id}", "updateIdentityProvider": "PATCH /control/v1/tenants/{id}/identity-providers/{provider_id}",
		"retireIdentityProvider": "POST /control/v1/tenants/{id}/identity-providers/{provider_id}:retire",
		"getAuthPolicy":          "GET /control/v1/tenants/{id}/auth-policy", "updateAuthPolicy": "PATCH /control/v1/tenants/{id}/auth-policy",
		"listTenantPrincipals": "GET /control/v1/tenants/{id}/principals", "bindPrincipal": "POST /control/v1/tenants/{id}/principals/{principal_id}:bind",
		"getTenantAiPolicy": "GET /control/v1/tenants/{id}/ai-policy", "suspendAi": "POST /control/v1/tenants/{id}/ai-policy:suspend",
		"resumeAi": "POST /control/v1/tenants/{id}/ai-policy:resume"}
	if len(ops) != len(want) {
		t.Fatalf("operations: got %d, want %d", len(ops), len(want))
	}
	for id, route := range want {
		t.Run(id, func(t *testing.T) {
			op := ops[id]
			if got := op.Method + " " + op.Path; got != route {
				t.Errorf("route: got %s, want %s", got, route)
			}
			if op.Permission != "" {
				t.Errorf("permission: got %s, want none", op.Permission)
			}
			if !op.AgentAllowed {
				t.Error("agent allowed: got false, want true")
			}
		})
	}
	if !strings.Contains(string(ControlSpec()), "\nopenapi: \"3.1") {
		t.Fatal("control spec")
	}
}

// TestModelOperations: the model gateway's one operation, permissioned and
// open to agents (ADR-0036).
func TestModelOperations(t *testing.T) {
	ops, err := ModelOperations()
	if err != nil {
		t.Fatal(err)
	}
	op, ok := ops["createModelMessage"]
	if len(ops) != 1 || !ok {
		t.Fatalf("operations: got %v, want createModelMessage alone", ops)
	}
	if got := op.Method + " " + op.Path; got != "POST /v1/model/v1/messages" {
		t.Errorf("route: got %s, want POST /v1/model/v1/messages", got)
	}
	if op.Permission != "agents.model.invoke" || !op.AgentAllowed {
		t.Errorf("operation: got permission %q, agents %v, want agents.model.invoke, open to agents", op.Permission, op.AgentAllowed)
	}
	if !strings.Contains(string(ModelSpec()), "\nopenapi: \"3.1") {
		t.Fatal("model spec")
	}
	if len(MustModelOperations()) != 1 {
		t.Error("MustModelOperations: got other than the one operation")
	}
}
