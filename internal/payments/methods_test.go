package payments_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

func paymentMethods() payments.PaymentMethods {
	return payments.PaymentMethods{LegalEntities: tenancy.Directory{}}
}

// methodsOf are the legal entity's payment methods and rankings as
// "code description active run-order" and "method currency banks active".
func methodsOf(t *testing.T, le uuid.UUID) (methods, rankings []string) {
	t.Helper()
	q := func(sql string) []string {
		rows, err := pgtest.Admin(t).Query(context.Background(), sql, le)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		return out
	}
	return q(`SELECT code || ' ' || description || ' ' || is_active || ' ' || coalesce(run_order::text, '-') FROM payments.payment_method
			WHERE legal_entity_id = $1 ORDER BY code`),
		q(`SELECT method || ' ' || currency || ' ' || array_to_string(house_banks, ',') || ' ' || is_active FROM payments.bank_ranking
			WHERE legal_entity_id = $1 ORDER BY method, currency`)
}

// TestPaymentMethods: the configuration's payment methods and rankings are
// kept per legal entity; changed ones are updated, removed ones become
// inactive and come back when the document has them again, and each
// method takes its place in the company's run order: by its order where
// the document gives one, else as listed (ADR-0038).
func TestPaymentMethods(t *testing.T) {
	e := setup(t)
	m := paymentMethods()
	apply := func(doc *config.Config) {
		t.Helper()
		if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error { return m.Apply(ctx, doc) }); err != nil {
			t.Fatal(err)
		}
	}
	want := func(step string, methods, rankings []string) {
		t.Helper()
		gotM, gotR := methodsOf(t, e.US)
		if !slices.Equal(gotM, methods) {
			t.Errorf("%s, methods: got %v, want %v", step, gotM, methods)
		}
		if !slices.Equal(gotR, rankings) {
			t.Errorf("%s, rankings: got %v, want %v", step, gotR, rankings)
		}
	}
	want("from the first configuration", []string{"T Bank transfer true 1"}, []string{"T USD MAIN true"})

	doc := fixture.Document(t)
	// The rankings name house banks by code; HouseBanks keeps those.
	doc.Companies[0].PaymentMethods[0].Description = "Wire"
	doc.Companies[0].PaymentMethods[0].Ranking = []config.BankRanking{{Currency: "USD", HouseBanks: []string{"HSBC", "MAIN"}},
		{Currency: "EUR", HouseBanks: []string{"MAIN"}}}
	doc.Companies[0].PaymentMethods = append(doc.Companies[0].PaymentMethods,
		config.PaymentMethod{Code: "C", Description: "Cheque", Ranking: []config.BankRanking{{Currency: "USD", HouseBanks: []string{"MAIN"}}}})
	apply(doc)
	want("changed", []string{"C Cheque true 2", "T Wire true 1"}, []string{"C USD MAIN true", "T EUR MAIN true", "T USD HSBC,MAIN true"})

	// Only the order changes: the cheque is tried first.
	doc.Companies[0].PaymentMethods[1].Order = 1
	apply(doc)
	want("reordered", []string{"C Cheque true 1", "T Wire true 2"}, []string{"C USD MAIN true", "T EUR MAIN true", "T USD HSBC,MAIN true"})

	// An inactive method keeps the place it had; it is not tried.
	apply(fixture.Document(t))
	want("back to the first", []string{"C Cheque false 1", "T Bank transfer true 1"}, []string{"C USD MAIN false", "T EUR MAIN false", "T USD MAIN true"})

	var active, inactive bool
	if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) (err error) {
		if active, err = m.IsPaymentMethod(ctx, e.US, "T"); err != nil {
			return err
		}
		inactive, err = m.IsPaymentMethod(ctx, e.US, "C")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !active {
		t.Error("T: got not a payment method, want one")
	}
	if inactive {
		t.Error("C, removed: got a payment method, want none")
	}
	if f, err := m.Check(context.Background(), nil, doc); f != nil || err != nil {
		t.Errorf("check: got %v, %v, want nothing", f, err)
	}
}

type brokenEntities struct{}

var errDirectory = errors.New("directory down")

func (brokenEntities) EntityByCode(context.Context, string) (tenancy.Entity, bool, error) {
	return tenancy.Entity{}, false, errDirectory
}

// TestPaymentMethodsRefused: a company without a legal entity, or a
// directory that fails, stops the activation.
func TestPaymentMethodsRefused(t *testing.T) {
	e := setup(t)
	doc := fixture.Document(t)
	doc.Companies[0].Code = "9999"
	for name, c := range map[string]struct {
		m   payments.PaymentMethods
		doc *config.Config
	}{
		"no legal entity": {paymentMethods(), doc},
		"directory fails": {payments.PaymentMethods{LegalEntities: brokenEntities{}}, fixture.Document(t)},
	} {
		t.Run(name, func(t *testing.T) {
			err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error { return c.m.Apply(ctx, c.doc) })
			if err == nil {
				t.Fatal("applied, want refused")
			}
		})
	}
}
