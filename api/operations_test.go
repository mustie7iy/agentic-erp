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
	if !ok || cancel.Method != "POST" || cancel.Path != "/v1/operations/{id}:cancel" ||
		cancel.Permission != "platform.operation.cancel" || !cancel.AgentAllowed {
		t.Fatalf("cancelOperation: %+v", cancel)
	}
	if approve := ops["approveApprovalRequest"]; approve.AgentAllowed {
		t.Fatal("approval decisions must be refused to agents")
	}
}

func TestOperationsRejectsBadContracts(t *testing.T) {
	for name, src := range map[string]string{
		"not yaml":      "paths: [",
		"no permission": "paths:\n  /v1/x:\n    get:\n      operationId: getX\n",
		"bad operation": "paths:\n  /v1/x:\n    get: [1]\n",
	} {
		if _, err := parseOperations([]byte(src), true); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A contract without permissions (the control plane) still needs operationIds.
	if ops, err := parseOperations([]byte("paths:\n  /v1/x:\n    get:\n      operationId: getX\n"), false); err != nil || len(ops) != 1 {
		t.Fatal(ops, err)
	}
	if _, err := parseOperations([]byte("paths:\n  /v1/x:\n    get:\n      summary: x\n"), false); err == nil {
		t.Fatal("operation without operationId accepted")
	}
	ops, err := parseOperations([]byte("paths:\n  /v1/x:\n    parameters: []\n    get:\n      operationId: getX\n      x-permission: a.b.c\n      x-agent: false\n"), true)
	if err != nil || len(ops) != 1 || ops["getX"].AgentAllowed || !strings.HasPrefix(ops["getX"].Path, "/v1/") {
		t.Fatal(ops, err)
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
		t.Fatalf("%d operations", len(ops))
	}
	for id, route := range want {
		if op := ops[id]; op.Method+" "+op.Path != route || op.Permission != "" || !op.AgentAllowed {
			t.Errorf("%s: %+v", id, op)
		}
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
		"rotateTenantKey": "POST /control/v1/tenants/{id}:rotateKey", "eraseTenant": "POST /control/v1/tenants/{id}:erase", "getTenantFingerprint": "GET /control/v1/tenants/{id}/fingerprint"}
	if len(ops) != len(want) {
		t.Fatalf("%d operations", len(ops))
	}
	for id, route := range want {
		if op := ops[id]; op.Method+" "+op.Path != route || op.Permission != "" || !op.AgentAllowed {
			t.Errorf("%s: %+v", id, op)
		}
	}
	if !strings.Contains(string(ControlSpec()), "\nopenapi: \"3.1") {
		t.Fatal("control spec")
	}
}
