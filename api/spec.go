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
	// Lifecycle is the operation's deprecation, zero while it is not
	// deprecated (ADR-0041).
	Lifecycle Lifecycle
	// Stability is x-stability-level: draft, alpha, beta or stable (the
	// default). Breaking changes to beta operations may be accepted after
	// the first partner consumes /v1; to stable ones never (ADR-0041).
	Stability string
}

// Lifecycle is a deprecated operation's schedule: deprecated: true with
// x-deprecated-at, x-sunset and externalDocs (ADR-0041).
type Lifecycle struct {
	Deprecated   bool
	DeprecatedAt time.Time // x-deprecated-at: the Deprecation header (RFC 9745)
	Sunset       time.Time // x-sunset: the Sunset header (RFC 8594)
	Link         string    // externalDocs.url: the Link rel="deprecation" target
}

// MinSunsetNotice is the least time between deprecating an operation and
// removing it (ADR-0041); oasdiff's --deprecation-days-stable holds the same.
const MinSunsetNotice = 365 * 24 * time.Hour

// operationNode is an operation's fields that the request pipeline reads.
type operationNode struct {
	ID           string `yaml:"operationId"`
	Permission   string `yaml:"x-permission"`
	Agent        *bool  `yaml:"x-agent"`
	Autonomy     string `yaml:"x-autonomy"`
	Deprecated   bool   `yaml:"deprecated"`
	DeprecatedAt string `yaml:"x-deprecated-at"`
	Sunset       string `yaml:"x-sunset"`
	Stability    string `yaml:"x-stability-level"`
	ExternalDocs struct {
		URL string `yaml:"url"`
	} `yaml:"externalDocs"`
}

// decodeOperation reads one operation; permissioned says whether it must
// declare x-permission.
func decodeOperation(node yaml.Node, permissioned bool) (Operation, error) {
	var op operationNode
	if err := node.Decode(&op); err != nil {
		return Operation{}, fmt.Errorf("decode: %w", err)
	}
	if op.ID == "" || (permissioned && op.Permission == "") {
		return Operation{}, errors.New("operationId and x-permission are required")
	}
	if op.Autonomy != "" && op.Autonomy != "propose" && op.Autonomy != "read" {
		return Operation{}, errors.New("x-autonomy is propose, read or absent")
	}
	switch op.Stability {
	case "":
		op.Stability = "stable"
	case "draft", "alpha", "beta", "stable":
	default:
		return Operation{}, errors.New("x-stability-level is draft, alpha, beta, stable or absent")
	}
	life, err := lifecycle(op)
	if err != nil {
		return Operation{}, err
	}
	return Operation{
		ID: op.ID, Permission: op.Permission, AgentAllowed: op.Agent == nil || *op.Agent,
		Proposes: op.Autonomy == "propose", Reads: op.Autonomy == "read", Lifecycle: life, Stability: op.Stability,
	}, nil
}

// lifecycle checks a deprecation: all of its fields or none, dates as
// YYYY-MM-DD, and the sunset at least MinSunsetNotice after the deprecation.
func lifecycle(op operationNode) (Lifecycle, error) {
	if !op.Deprecated {
		if op.DeprecatedAt != "" || op.Sunset != "" {
			return Lifecycle{}, errors.New("x-deprecated-at and x-sunset need deprecated: true")
		}
		return Lifecycle{}, nil
	}
	at, err := time.Parse(time.DateOnly, op.DeprecatedAt)
	if err != nil {
		return Lifecycle{}, fmt.Errorf("x-deprecated-at: %w", err)
	}
	sunset, err := time.Parse(time.DateOnly, op.Sunset)
	if err != nil {
		return Lifecycle{}, fmt.Errorf("x-sunset: %w", err)
	}
	if sunset.Sub(at) < MinSunsetNotice {
		return Lifecycle{}, fmt.Errorf("x-sunset %s is less than 365 days after x-deprecated-at %s", op.Sunset, op.DeprecatedAt)
	}
	if op.ExternalDocs.URL == "" {
		return Lifecycle{}, errors.New("a deprecated operation links its migration notes in externalDocs.url")
	}
	return Lifecycle{Deprecated: true, DeprecatedAt: at, Sunset: sunset, Link: op.ExternalDocs.URL}, nil
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
			op, err := decodeOperation(node, permissioned)
			if err != nil {
				return nil, fmt.Errorf("api: %s %s: %w", method, path, err)
			}
			op.Method, op.Path = strings.ToUpper(method), path
			ops[op.ID] = op
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
