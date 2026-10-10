package api

import (
	_ "embed"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed openapi.yaml
var openapiYAML []byte

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
	return parseOperations(openapiYAML)
}

func parseOperations(src []byte) (map[string]Operation, error) {
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
			if op.ID == "" || op.Permission == "" {
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
	ops, err := parseOperations(src)
	if err != nil {
		panic(err)
	}
	return ops
}
