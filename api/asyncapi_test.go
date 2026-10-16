package api

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// asyncDoc is api/asyncapi.yaml decoded into generic maps.
type asyncDoc map[string]any

var (
	ceTypeRE = regexp.MustCompile(`^com\.example\.erp\.[a-z_]+\.[a-z_]+\.v[0-9]+$`)
	uuidRE   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

const envelopeTraitRef = "#/components/messageTraits/CloudEvent"

func loadAsyncAPI(t *testing.T) asyncDoc {
	t.Helper()
	b, err := os.ReadFile("asyncapi.yaml")
	if err != nil {
		t.Fatalf("read asyncapi.yaml: %v", err)
	}
	// Decode into a plain map: yaml.v3 reuses a named map type for nested
	// mappings, which would defeat the map[string]any assertions below.
	var m map[string]any
	if err := yaml.Unmarshal(b, &m); err != nil {
		t.Fatalf("parse asyncapi.yaml: %v", err)
	}
	return asyncDoc(m)
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

// resolve follows a local JSON pointer ("#/a/b/c") within the document.
func (d asyncDoc) resolve(t *testing.T, ref string) map[string]any {
	t.Helper()
	if !strings.HasPrefix(ref, "#/") {
		t.Fatalf("only local refs are allowed, got %q", ref)
	}
	var cur any = map[string]any(d)
	for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		m := asMap(cur)
		if m == nil {
			t.Fatalf("ref %q: %q is not inside an object", ref, part)
		}
		var ok bool
		if cur, ok = m[part]; !ok {
			t.Fatalf("ref %q does not resolve (missing %q)", ref, part)
		}
	}
	m := asMap(cur)
	if m == nil {
		t.Fatalf("ref %q does not point to an object", ref)
	}
	return m
}

// deref returns the object itself, or its target when it is a $ref.
func (d asyncDoc) deref(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	for i := 0; i < 10; i++ {
		ref, ok := m["$ref"].(string)
		if !ok {
			return m
		}
		m = d.resolve(t, ref)
	}
	t.Fatal("$ref chain too long")
	return nil
}

// headerTypeConsts returns every `type` const declared in a message's
// headers schema, looking through allOf.
func headerTypeConsts(t *testing.T, d asyncDoc, headers map[string]any) []string {
	t.Helper()
	var out []string
	var walk func(map[string]any)
	walk = func(s map[string]any) {
		if s == nil {
			return
		}
		s = d.deref(t, s)
		if c, ok := asMap(asMap(s["properties"])["type"])["const"].(string); ok {
			out = append(out, c)
		}
		for _, sub := range asSlice(s["allOf"]) {
			walk(asMap(sub))
		}
	}
	walk(headers)
	return out
}

func TestAsyncAPIVersion(t *testing.T) {
	d := loadAsyncAPI(t)
	v := asString(d["asyncapi"])
	if !strings.HasPrefix(v, "3.") {
		t.Fatalf("asyncapi version = %q, want 3.x", v)
	}
	desc := asString(asMap(d["info"])["description"])
	for _, want := range []string{"additive", ".v2", "side by side"} {
		if !strings.Contains(desc, want) {
			t.Errorf("info.description does not state the versioning rule (missing %q)", want)
		}
	}
	for name, s := range asMap(d["servers"]) {
		if p := asString(asMap(s)["protocol"]); p != "nats" {
			t.Errorf("server %s: protocol %q, want nats", name, p)
		}
	}
}

func TestAsyncAPIEnvelopeTrait(t *testing.T) {
	d := loadAsyncAPI(t)
	trait := d.resolve(t, envelopeTraitRef)
	headers := d.deref(t, asMap(trait["headers"]))
	props := asMap(headers["properties"])
	for _, attr := range []string{"specversion", "id", "source", "type", "subject", "time", "datacontenttype", "tenantid", "actorid", "traceid"} {
		if _, ok := props[attr]; !ok {
			t.Errorf("envelope headers lack CloudEvents attribute %q", attr)
		}
	}
	required := map[string]bool{}
	for _, r := range asSlice(headers["required"]) {
		required[asString(r)] = true
	}
	for _, attr := range []string{"specversion", "id", "source", "type", "subject", "time", "datacontenttype", "tenantid", "actorid"} {
		if !required[attr] {
			t.Errorf("envelope headers do not require %q", attr)
		}
	}
	if p := asString(asMap(props["type"])["pattern"]); p != ceTypeRE.String() {
		t.Errorf("envelope type pattern = %q, want %q", p, ceTypeRE.String())
	}
}

func TestAsyncAPIMessages(t *testing.T) {
	d := loadAsyncAPI(t)
	components := asMap(d["components"])
	messages := asMap(components["messages"])
	if len(messages) == 0 {
		t.Fatal("no messages in components")
	}

	names := map[string]string{}
	types := map[string]string{}
	for key, raw := range messages {
		msg := asMap(raw)
		name := asString(msg["name"])
		if name == "" {
			t.Errorf("message %s has no name", key)
		} else if prev, dup := names[name]; dup {
			t.Errorf("message name %q used by %s and %s", name, prev, key)
		} else {
			names[name] = key
		}
		for _, f := range []string{"summary", "description"} {
			if asString(msg[f]) == "" {
				t.Errorf("message %s has no %s", key, f)
			}
		}

		hasTrait := false
		for _, tr := range asSlice(msg["traits"]) {
			if asString(asMap(tr)["$ref"]) == envelopeTraitRef {
				hasTrait = true
			}
		}
		if !hasTrait {
			t.Errorf("message %s does not apply the %s trait", key, envelopeTraitRef)
		}

		payload := asMap(msg["payload"])
		if payload == nil {
			t.Errorf("message %s has no payload", key)
			continue
		}
		schema := d.deref(t, payload)
		if asString(schema["type"]) != "object" {
			t.Errorf("message %s payload is not an object schema", key)
		}

		consts := headerTypeConsts(t, d, asMap(msg["headers"]))
		if len(consts) != 1 {
			t.Errorf("message %s: want exactly one CloudEvents type const in headers, got %v", key, consts)
			continue
		}
		ceType := consts[0]
		if !ceTypeRE.MatchString(ceType) {
			t.Errorf("message %s: type %q does not match %s", key, ceType, ceTypeRE)
		}
		if prev, dup := types[ceType]; dup {
			t.Errorf("type %q used by %s and %s", ceType, prev, key)
		}
		types[ceType] = key

		examples := asSlice(msg["examples"])
		if len(examples) == 0 {
			t.Errorf("message %s has no example", key)
		}
		required := asSlice(schema["required"])
		props := asMap(schema["properties"])
		for i, ex := range examples {
			exm := asMap(ex)
			h := asMap(exm["headers"])
			if got := asString(h["type"]); got != ceType {
				t.Errorf("message %s example %d: type %q, want %q", key, i, got, ceType)
			}
			if !ceTypeRE.MatchString(asString(h["type"])) {
				t.Errorf("message %s example %d: type %q does not match %s", key, i, h["type"], ceTypeRE)
			}
			for _, attr := range []string{"specversion", "id", "source", "subject", "time", "datacontenttype", "tenantid", "actorid"} {
				if _, ok := h[attr]; !ok {
					t.Errorf("message %s example %d lacks header %q", key, i, attr)
				}
			}
			for _, attr := range []string{"id", "subject", "tenantid", "actorid"} {
				if v := asString(h[attr]); !uuidRE.MatchString(v) {
					t.Errorf("message %s example %d: %s %q is not a uuid", key, i, attr, v)
				}
			}
			ctx := strings.Split(ceType, ".")[3]
			if src := asString(h["source"]); !strings.HasSuffix(src, "/"+ctx) {
				t.Errorf("message %s example %d: source %q is not the %s context", key, i, src, ctx)
			}
			p := asMap(exm["payload"])
			if p == nil {
				t.Errorf("message %s example %d has no payload", key, i)
				continue
			}
			for _, r := range required {
				if _, ok := p[asString(r)]; !ok {
					t.Errorf("message %s example %d: payload lacks required field %q", key, i, r)
				}
			}
			for f := range p {
				if _, ok := props[f]; !ok {
					t.Errorf("message %s example %d: payload field %q is not in the schema", key, i, f)
				}
			}
		}
	}
	t.Logf("%d messages, %d event types", len(messages), len(types))
}

func TestAsyncAPIChannels(t *testing.T) {
	d := loadAsyncAPI(t)
	channels := asMap(d["channels"])
	wantContexts := []string{"tenancy", "identity", "configuration", "partner", "ledger", "procurement", "payables", "payments", "workflow", "integration", "agents", "platform"}
	for _, c := range wantContexts {
		if _, ok := channels[c]; !ok {
			t.Errorf("missing channel for context %q", c)
		}
	}

	onChannel := map[string]string{} // component message ref -> channel
	for chName, raw := range channels {
		ch := asMap(raw)
		addr := asString(ch["address"])
		segs := strings.Split(addr, ".")
		if len(segs) != 5 {
			t.Errorf("channel %s: address %q has %d segments, want erp.{cell}.<context>.{tenant}.{event}", chName, addr, len(segs))
			continue
		}
		wellFormed := true
		for i, want := range map[int]string{0: "erp", 1: "{cell}", 3: "{tenant}", 4: "{event}"} {
			if segs[i] != want {
				t.Errorf("channel %s: address %q segment %d: got %q, want %q", chName, addr, i, segs[i], want)
				wellFormed = false
			}
		}
		if !wellFormed {
			continue
		}
		ctx := segs[2]
		if ctx != chName {
			t.Errorf("channel %s: address context %q differs from channel name", chName, ctx)
		}
		msgs := asMap(ch["messages"])
		if len(msgs) == 0 {
			t.Errorf("channel %s carries no messages", chName)
		}
		for _, mraw := range msgs {
			ref := asString(asMap(mraw)["$ref"])
			msg := d.resolve(t, ref)
			if prev, dup := onChannel[ref]; dup {
				t.Errorf("message %s is on channels %s and %s", ref, prev, chName)
			}
			onChannel[ref] = chName
			for _, ceType := range headerTypeConsts(t, d, asMap(msg["headers"])) {
				if parts := strings.Split(ceType, "."); len(parts) < 4 || parts[3] != ctx {
					t.Errorf("channel %s carries %s whose type %q is not in context %q", chName, ref, ceType, ctx)
				}
			}
			for i, ex := range asSlice(msg["examples"]) {
				ceType := asString(asMap(asMap(ex)["headers"])["type"])
				if parts := strings.Split(ceType, "."); len(parts) < 4 || parts[3] != ctx {
					t.Errorf("channel %s carries %s whose example %d type %q is not in context %q", chName, ref, i, ceType, ctx)
				}
			}
		}
	}
	for key := range asMap(asMap(d["components"])["messages"]) {
		if _, ok := onChannel["#/components/messages/"+key]; !ok {
			t.Errorf("message %s is not on any channel", key)
		}
	}
}

func TestAsyncAPIOperations(t *testing.T) {
	d := loadAsyncAPI(t)
	sent := map[string]bool{} // channel/message refs covered by a send operation
	for opName, raw := range asMap(d["operations"]) {
		op := asMap(raw)
		if a := asString(op["action"]); a != "send" {
			t.Errorf("operation %s: action %q, want send", opName, a)
		}
		chRef := asString(asMap(op["channel"])["$ref"])
		d.resolve(t, chRef)
		for _, m := range asSlice(op["messages"]) {
			ref := asString(asMap(m)["$ref"])
			if !strings.HasPrefix(ref, chRef+"/messages/") {
				t.Errorf("operation %s: message %q is not a message of its channel %q", opName, ref, chRef)
			}
			d.resolve(t, ref)
			sent[ref] = true
		}
	}
	var missing []string
	for chName, raw := range asMap(d["channels"]) {
		for key := range asMap(asMap(raw)["messages"]) {
			if ref := "#/channels/" + chName + "/messages/" + key; !sent[ref] {
				missing = append(missing, ref)
			}
		}
	}
	sort.Strings(missing)
	for _, m := range missing {
		t.Errorf("no send operation for %s", m)
	}
}

// TestAsyncAPIRequiredEvents pins the minimum event catalogue.
func TestAsyncAPIRequiredEvents(t *testing.T) {
	d := loadAsyncAPI(t)
	messages := asMap(asMap(d["components"])["messages"])
	for _, name := range []string{
		"LegalEntityCreated", "RoleAssigned", "AccessRevoked", "ConfigurationActivated",
		"ChangeRequestSubmitted", "ChangeRequestApproved", "BusinessPartnerCreated", "SupplierApproved",
		"BankAccountChanged", "PartnerBlocked", "JournalEntryPosted", "JournalEntryReversed", "ItemsCleared",
		"PeriodOpened", "PeriodClosed", "PeriodLocked", "RequisitionSubmitted", "PurchaseOrderReleased", "GoodsReceived",
		"GoodsReceiptReversed", "SupplierInvoiceCaptured", "SupplierInvoiceMatched", "InvoiceHeld",
		"SupplierInvoicePosted", "SupplierInvoiceCancelled", "SupplierInvoiceReversed", "PaymentRunProposed", "PaymentRunReleased",
		"PaymentRunRejected", "BankFileTransmitted", "ApprovalRequested", "ApprovalCompleted",
		"MigrationRunCompleted", "RecommendationMade", "OperationCompleted",
		"LegalEntityBlocked", "LegalEntityUnblocked", "OrgUnitCreated", "OrgUnitBlocked", "OrgUnitUnblocked",
		"CompanyAccessGranted", "SodRuleChanged", "ChangeRequestValidated", "ChangeRequestRejected",
		"ChangeRequestWithdrawn", "PartnerUnblocked", "RequisitionCancelled", "PurchaseOrderCancelled",
		"ContractActivated", "ContractTerminated", "ContractCancelled", "PaymentRunFailed",
		"ConnectorEnabled", "ConnectorDisabled", "ExchangeRateMaintained",
		"RequisitionApproved", "RequisitionRejected", "RequisitionOrdered", "PurchaseOrderClosed", "ContractExpired",
		"InvoiceHoldReleased", "SupplierInvoiceRejected", "BankFileCreated", "BankFileAcknowledged", "BankFileRejected",
		"PartnerChangeRejected", "PartnerChangeCancelled", "BusinessPartnerUpdated", "PrincipalCreated",
		"PrincipalBlocked", "PrincipalDeprovisioned", "AgentProfileChanged", "RoleCreated", "RoleDeactivated",
		"RolePermissionGranted", "RolePermissionEnded", "TenantStatusChanged", "RecommendationDecided",
	} {
		if _, ok := messages[name]; !ok {
			t.Errorf("missing event %s", name)
		}
	}
	outcome := asMap(asMap(d.resolve(t, "#/components/schemas/ApprovalCompletedV1")["properties"])["outcome"])
	if outcome == nil {
		t.Error("ApprovalCompleted payload has no outcome")
	}
}
