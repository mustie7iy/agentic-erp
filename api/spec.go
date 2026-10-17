package api

import (
	_ "embed"
	"fmt"
	"strings"

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
			var op struct {
				ID         string `yaml:"operationId"`
				Permission string `yaml:"x-permission"`
				Agent      *bool  `yaml:"x-agent"`
			}
			if err := node.Decode(&op); err != nil {
				return nil, fmt.Errorf("api: %s %s: %w", method, path, err)
			}
			if op.ID == "" || (permissioned && op.Permission == "") {
				return nil, fmt.Errorf("api: %s %s: operationId and x-permission are required", method, path)
			}
			ops[op.ID] = Operation{
				ID: op.ID, Method: strings.ToUpper(method), Path: path, Permission: op.Permission,
				AgentAllowed: op.Agent == nil || *op.Agent,
			}
		}
	}
	return ops, nil
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
