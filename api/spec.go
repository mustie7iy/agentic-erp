package api

import (
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed openapi.yaml
var openapiYAML []byte

//go:embed control.yaml
var controlYAML []byte

//go:embed directory.yaml
var directoryYAML []byte

//go:embed model.yaml
var modelYAML []byte

// Operation is what the request pipeline needs to know about an operation
// beyond what the generated server enforces.
type Operation struct {
	ID           string // operationId
	Method       string // upper case
	Path         string
	Permission   string // x-permission
	AgentAllowed bool   // false when x-agent: false
	// Proposes is x-autonomy: propose: the operation only proposes, and an
	// agent of autonomy level 1 may perform it (ADR-0036).
	Proposes bool
	// Reads is x-autonomy: read: the operation changes no business data though it is
	// not a GET, and an agent of any level may perform it.
	Reads bool
	// Class is the operation's latency class (ADR-0043): one of Classes.
	Class string
}

// Latency classes (ADR-0043): each has its p95/p99 objective in
// 01-requirements §1.3, and erp_operation_info maps every operation to one.
const (
	ClassRead     = "read"     // a GET of one resource
	ClassList     = "list"     // any other GET
	ClassWrite    = "write"    // any other method
	ClassApproval = "approval" // x-slo-class: approval
	ClassReport   = "report"   // x-slo-class: report
)

// Classes are the latency classes, a bounded enumeration.
var Classes = []string{ClassRead, ClassList, ClassWrite, ClassApproval, ClassReport}

// class is the latency class an operation declares with x-slo-class, or
// the one its method and path imply.
func class(method, path, declared string) (string, error) {
	switch {
	case declared == ClassApproval || declared == ClassReport:
		return declared, nil
	case declared != "":
		return "", fmt.Errorf("x-slo-class is approval, report or absent, not %q", declared)
	case method != "get":
		return ClassWrite, nil
	case strings.HasSuffix(path, "}"):
		return ClassRead, nil
	}
	return ClassList, nil
}

// Autonomy levels (ADR-0036): what an agent of the level performs beyond
// its permissions.
const (
	LevelReads    = 0 // reads
	LevelProposes = 1 // also the operations that only propose (x-autonomy: propose)
	LevelWrites   = 2 // any write its permissions allow; 3 is reserved and acts as 2
)

// Level is the autonomy level an agent needs for the operation: GET, HEAD
// and OPTIONS run read-only and x-autonomy: read changes no business data;
// x-autonomy: propose only proposes; every other operation writes. Authz
// refuses an agent below it, and agents are offered only the tools their
// level performs.
func (o Operation) Level() int {
	switch {
	case o.Method == "GET" || o.Method == "HEAD" || o.Method == "OPTIONS" || o.Reads:
		return LevelReads
	case o.Proposes:
		return LevelProposes
	}
	return LevelWrites
}

// Operations returns every operation in the contract by operationId.
func Operations() (map[string]Operation, error) {
	return parseOperations(openapiYAML, true)
}

// parseOperations reads the operations of a contract; permissioned says
// whether each must declare x-permission.
func parseOperations(src []byte, permissioned bool) (map[string]Operation, error) {
	var doc struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, fmt.Errorf("api: parse contract: %w", err)
	}
	ops := map[string]Operation{}
	for path, item := range doc.Paths {
		for method, node := range item {
			switch method {
			case "get", "post", "patch", "put", "delete":
			default:
				continue
			}
			op, err := parseOperation(method, path, node, permissioned)
			if err != nil {
				return nil, fmt.Errorf("api: %s %s: %w", method, path, err)
			}
			ops[op.ID] = op
		}
	}
	return ops, nil
}

// parseOperation reads one operation of a contract.
func parseOperation(method, path string, node yaml.Node, permissioned bool) (Operation, error) {
	var op struct {
		ID         string `yaml:"operationId"`
		Permission string `yaml:"x-permission"`
		Agent      *bool  `yaml:"x-agent"`
		Autonomy   string `yaml:"x-autonomy"`
		Class      string `yaml:"x-slo-class"`
	}
	if err := node.Decode(&op); err != nil {
		return Operation{}, fmt.Errorf("decode: %w", err)
	}
	if op.ID == "" || (permissioned && op.Permission == "") {
		return Operation{}, errors.New("operationId and x-permission are required")
	}
	if op.Autonomy != "" && op.Autonomy != "propose" && op.Autonomy != "read" {
		return Operation{}, errors.New("x-autonomy is propose, read or absent")
	}
	cls, err := class(method, path, op.Class)
	if err != nil {
		return Operation{}, err
	}
	return Operation{
		ID: op.ID, Method: strings.ToUpper(method), Path: path, Permission: op.Permission,
		AgentAllowed: op.Agent == nil || *op.Agent, Proposes: op.Autonomy == "propose", Reads: op.Autonomy == "read",
		Class: cls,
	}, nil
}

// MustOperations is Operations for program start: the embedded contract is
// checked by this package's tests, so failing to parse it is a build defect.
func MustOperations() map[string]Operation {
	return mustParse(openapiYAML)
}

func mustParse(src []byte) map[string]Operation {
	ops, err := parseOperations(src, true)
	if err != nil {
		//nolint:forbidigo // init-time wiring of a static value: the embedded contract is fixed at build time and checked by this package's tests
		panic(err)
	}
	return ops
}

// Spec is the embedded contract, for clients that build on it (the agent
// tools are generated from its operations).
func Spec() []byte { return openapiYAML }

// ControlSpec is the embedded control-plane contract (control.yaml): the
// operators' API for tenants, served on the admin listener by hand-written
// handlers (internal/controlplane), which its tests check against it.
func ControlSpec() []byte { return controlYAML }

// ControlOperations returns every operation of the control-plane contract
// by operationId. Its operations declare no x-permission: operators are not
// principals, and any operator the verifier accepts may call any of them.
func ControlOperations() (map[string]Operation, error) {
	return parseOperations(controlYAML, false)
}

// DirectorySpec is the embedded global control-plane contract
// (directory.yaml): the operators' API across cells, served by erp-router
// on its admin listener by hand-written handlers (internal/globalplane).
func DirectorySpec() []byte { return directoryYAML }

// DirectoryOperations returns every operation of the global control-plane
// contract by operationId; like the control plane's, they declare no
// x-permission.
func DirectoryOperations() (map[string]Operation, error) {
	return parseOperations(directoryYAML, false)
}

// ModelCallTimeout is how long a model call through the gateway may take:
// the provider's client gives up after it, and the cell and the router keep
// the answer's connection open for it and a minute more, past their
// listeners' write timeouts (ADR-0036). It is here, beside the gateway's
// contract, because the cell and the router share it.
const ModelCallTimeout = 10 * time.Minute

// ModelSpec is the embedded model gateway contract (model.yaml): every
// model call, served on the public listener outside the generated server
// by hand-written handlers (internal/agents), ADR-0036.
func ModelSpec() []byte { return modelYAML }

// MustModelOperations is ModelOperations for program start, as
// MustOperations is for the public API's.
func MustModelOperations() map[string]Operation { return mustParse(modelYAML) }

// ModelOperations returns the model gateway's operations by operationId;
// they declare their x-permission, as the public API's do.
func ModelOperations() (map[string]Operation, error) {
	return parseOperations(modelYAML, true)
}
