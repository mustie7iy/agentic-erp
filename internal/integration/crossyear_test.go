package integration_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// yearsSetup is setup with company 2000's journal number ranges, and so
// its periods, in 2025 too, so runs of 2025 and 2026 load one after the
// other; its directory holds no extract.
func yearsSetup(t *testing.T) env {
	t.Helper()
	f := fixture.NewWith(t, nil, func(c *config.Config) {
		for _, dt := range []string{"journal_re", "journal_kg", "journal_kz", "journal_ob"} {
			c.NumberRanges = append(c.NumberRanges, config.NumberRange{Company: "2000", DocumentType: dt, FiscalYear: 2025, First: 100000, Last: 999999})
		}
	}, perms...)
	dir := t.TempDir()
	f.API = apitest.NewWith(t, func(c *app.Config) { c.ExtractDir = dir })
	return env{Env: f, dir: dir}
}

// runIn creates a run of company 2000 for a fiscal year.
func (e env) runIn(t *testing.T, connector, mode string, year int) map[string]any {
	t.Helper()
	r, _ := e.Must(201, "POST", "/v1/migration-runs", fmt.Sprintf(`{"connector_id": %q, "scope": {"legal_entity_ids": [%q], "fiscal_year": %d},
		"mode": %q, "is_rehearsal": false}`, connector, e.DE, year, mode), "")
	return r
}

// acrossYears is tiny's vendor in company 2000 with SAP's journal of 2025
// and 2026: an invoice of 2025 (0100000002) SAP paid in 2026 by a payment
// of the same number in that year, which cleared both (AUGGJ 2026; the
// invoice's line without its clearing date), an invoice of 2025 still
// open, and an invoice and a credit memo of 2025 SAP cleared against each
// other on 5 February 2026 by a clearing document of no line
// (0100000009).
func acrossYears() *sap.Extract {
	x := tiny()
	x.EKKO, x.EKPO, x.MSEG, x.RBKP, x.RSEG = nil, nil, nil, nil, nil
	line := func(gjahr, belnr, docln, racct, hsl, budat, blart string) sap.ACDOCA {
		return sap.ACDOCA{RLDNR: "0L", RBUKRS: "2000", RWCUR: "EUR", GJAHR: gjahr, BELNR: belnr, DOCLN: docln, RACCT: racct, HSL: hsl, WSL: hsl,
			BUDAT: budat, BLART: blart}
	}
	vendor := func(l sap.ACDOCA, augbl, augdt, auggj string) sap.ACDOCA {
		l.LIFNR, l.AUGBL, l.AUGDT, l.AUGGJ = "0000000001", augbl, augdt, auggj
		return l
	}
	x.ACDOCA = []sap.ACDOCA{
		line("2025", "0100000002", "000001", "0000520000", "80.00", "20251110", "RE"),
		vendor(line("2025", "0100000002", "000002", "0000211000", "-80.00", "20251110", "RE"), "0100000002", "", "2026"),
		line("2025", "0100000003", "000001", "0000520000", "30.00", "20251120", "RE"),
		vendor(line("2025", "0100000003", "000002", "0000211000", "-30.00", "20251120", "RE"), "", "", ""),
		line("2025", "0100000004", "000001", "0000520000", "20.00", "20251201", "RE"),
		vendor(line("2025", "0100000004", "000002", "0000211000", "-20.00", "20251201", "RE"), "0100000009", "20260205", "2026"),
		line("2025", "0100000005", "000001", "0000520000", "-20.00", "20251215", "KG"),
		vendor(line("2025", "0100000005", "000002", "0000211000", "20.00", "20251215", "KG"), "0100000009", "20260205", "2026"),
		vendor(line("2026", "0100000002", "000001", "0000211000", "80.00", "20260115", "KZ"), "0100000002", "20260115", "2026"),
		line("2026", "0100000002", "000002", "0000100000", "-80.00", "20260115", "KZ"),
	}
	return x
}

// TestClearedInALaterYear: a fiscal year's run holds open at the year's
// end the vendor items SAP cleared in a later fiscal year, in either
// mode, and reconciles; the later year's run clears them, with its
// payment, or by SAP's clearing date when no document of the clearing is
// of its year, against the entries (documents) or the opening entry's
// lines (balances) the earlier run loaded them as, and reconciles; the
// same extract again records nothing (ADR-0034 As built, items cleared in
// a later fiscal year).
func TestClearedInALaterYear(t *testing.T) {
	for _, mode := range []string{"documents", "balances"} {
		t.Run("after a "+mode+" run", func(t *testing.T) {
			e := yearsSetup(t)
			write(t, e.dir, "years.json", acrossYears())
			conn := e.connector(t, "years.json")
			first := e.runIn(t, conn, mode, 2025)
			if _, got := e.act(t, first, "start"); got["status"] != "loaded" {
				t.Fatalf("2025's run: got %v, want loaded: %v", got["status"], got["report"])
			}
			if n := count(t, `SELECT count(*) FROM ledger.clearing_item WHERE tenant_id = $1`, e.Tenant.ID); n != 0 {
				t.Errorf("items 2025's run cleared: got %d, want none: SAP cleared them in 2026", n)
			}
			e.openPayablesAgree(t, first)
			later := e.runIn(t, conn, "documents", 2026)
			_, got := e.act(t, later, "start")
			if got["status"] != "loaded" {
				t.Fatalf("2026's run: got %v, want loaded: %v", got["status"], got["report"])
			}
			if loaded := fmt.Sprint(report(got)["loaded"]); loaded != "map[clearings:2 journal_entries:1]" {
				t.Errorf("loaded by 2026's run: got %s, want the payment and both clearings", loaded)
			}
			if n := count(t, `SELECT count(*) FROM ledger.clearing_item WHERE tenant_id = $1`, e.Tenant.ID); n != 4 {
				t.Errorf("cleared items: got %d, want the payment and the three items of 2025", n)
			}
			var dates []string
			for _, d := range []string{"2026-01-15", "2026-02-05"} {
				if count(t, `SELECT count(*) FROM ledger.clearing WHERE tenant_id = $1 AND clearing_date = $2`, e.Tenant.ID, d) == 1 {
					dates = append(dates, d)
				}
			}
			if len(dates) != 2 {
				t.Errorf("clearing dates: got %v, want the payment's and SAP's clearing date of the clearing of no line", dates)
			}
			e.openPayablesAgree(t, later)
			again := e.runIn(t, conn, "documents", 2026)
			if _, got = e.act(t, again, "start"); got["status"] != "loaded" || len(report(got)["loaded"].(map[string]any)) != 0 {
				t.Errorf("the same extract again: got %v, %v, want loaded with nothing", got["status"], report(got)["loaded"])
			}
		})
	}
}

// TestClearedInALaterYearRefused: a later year's run refuses a clearing
// of an earlier year's item no run of that year loaded, naming the year
// to migrate first, and the quality gate refuses such a clearing in a
// balances run, which cannot clear it, one of earlier years' documents
// alone that the extract gives no clearing date for, and a clearing year
// that is no year.
func TestClearedInALaterYearRefused(t *testing.T) {
	t.Run("no run of the earlier year", func(t *testing.T) {
		e := yearsSetup(t)
		write(t, e.dir, "years.json", acrossYears())
		_, got := e.act(t, e.runIn(t, e.connector(t, "years.json"), "documents", 2026), "start")
		want := "ACDOCA clearing 2000/0100000002: SAP cleared item 2000/2025/0100000002/000002 of fiscal year 2025 with it, " +
			"which no run of that year loaded: migrate fiscal year 2025 first"
		if load := loadIssues(got); got["status"] != "failed" || !slices.Contains(load, want) {
			t.Errorf("2026's run: got %v, %q, want failed with %q", got["status"], load, want)
		}
	})
	noYear := acrossYears()
	noYear.ACDOCA[8].AUGGJ = "20x6"
	undated := acrossYears()
	undated.ACDOCA[5].AUGDT, undated.ACDOCA[7].AUGDT = "", ""
	for name, c := range map[string]struct {
		extract *sap.Extract
		mode    string
		want    string
	}{
		"a balances run": {acrossYears(), "balances", "SAP cleared items of an earlier fiscal year (2000/2025/0100000002/000002) with it in 2026, " +
			"which a balances run cannot clear on the platform: load the year in documents mode"},
		"no clearing year": {noYear, "documents", `the clearing's fiscal year (AUGGJ) "20x6" is no fiscal year`},
		"no document of the year nor a clearing date": {undated, "documents", "SAP cleared items of an earlier fiscal year " +
			"(2000/2025/0100000004/000002, 2000/2025/0100000005/000002) with it in 2026, and none of its documents is of that year nor its lines " +
			"carry SAP's clearing date (AUGDT), which the run dates the clearing by: extract AUGDT"},
	} {
		t.Run(name, func(t *testing.T) {
			e := yearsSetup(t)
			write(t, e.dir, "years.json", c.extract)
			_, got := e.act(t, e.runIn(t, e.connector(t, "years.json"), c.mode, 2026), "start")
			if quality := fmt.Sprint(report(got)["quality"]); got["status"] != "failed" || !strings.Contains(quality, c.want) {
				t.Errorf("2026's run: got %v, %s, want failed with %q", got["status"], quality, c.want)
			}
		})
	}
}

// TestClearedInALaterYearFailures: the opening entry's items and their
// read by a later year's run fail with each of their statements.
func TestClearedInALaterYearFailures(t *testing.T) {
	e := yearsSetup(t)
	write(t, e.dir, "years.json", acrossYears())
	conn := e.connector(t, "years.json")
	h := endpoints(e.dir)
	yes := func(int32) error { return nil }
	first := e.runIn(t, conn, "balances", 2025)
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "2025's opening entry", func(ctx context.Context) error {
		_, err := h.StartRun(ctx, uuid.MustParse(first["id"].(string)), yes)
		return err
	})
	if _, got := e.act(t, first, "start"); got["status"] != "loaded" {
		t.Fatalf("2025's run: got %v, want loaded: %v", got["status"], got["report"])
	}
	later := e.runIn(t, conn, "documents", 2026)
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "2026's clearings", func(ctx context.Context) error {
		_, err := h.StartRun(ctx, uuid.MustParse(later["id"].(string)), yes)
		return err
	})
}
