package ledger_test

import (
	"context"
	"fmt"
	"maps"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
)

// TestAgingBucketAsTheList (T75e): ledger.AgingBucket and
// ledger.AgingRange, which the AR aging reads items into buckets with,
// put every item where the open items' aging counts it in SQL
// (agingSelect), on every key date and for every set of days: items not
// yet due, due on the key date, overdue on a bucket's first and last day
// and past the last bucket, and an item without terms, which the list
// shows without a due date. The reader of customer items parts from the
// list there (PO 2026-11-12): for it the item is due on its document date
// and ages from the day after.
func TestAgingBucketAsTheList(t *testing.T) {
	e := setup(t)
	if err := apply(t, e, withReceivables(t)); err != nil {
		t.Fatal(err)
	}
	north := e.newCustomer(t, "BP00000021", true)
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		for i, baseline := range []string{"2026-03-01", "2026-02-01", "2026-01-01", ""} {
			var terms *ledger.ItemTerms
			if baseline != "" {
				terms = &ledger.ItemTerms{PaymentTermCode: "NT30", BaselineDate: date(baseline), NetDays: 30}
			}
			if _, err := postings().Post(ctx, customerPosting(t, e, north, int64(1000*(i+1)), "2026-03-0"+fmt.Sprint(i+1), terms)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	items := receivable(t, e, north)
	if len(items) != 4 {
		t.Fatalf("open customer items: got %d, want 4", len(items))
	}
	// The items are due on 31 March, 3 March and 31 January.
	for _, c := range []struct {
		key  string
		days []int32
	}{
		{"2026-01-31", nil}, {"2026-02-01", nil}, {"2026-03-03", nil}, {"2026-03-31", nil}, {"2026-04-01", nil}, {"2026-04-30", nil}, {"2026-05-01", nil},
		{"2026-03-31", []int32{28, 59}}, {"2026-03-31", []int32{27, 58}}, {"2026-03-31", []int32{1}}, {"2027-12-31", []int32{10, 20, 30, 40, 50, 3650}},
	} {
		query := "&aging_key_date=" + c.key
		for i, d := range c.days {
			query += map[bool]string{true: "&aging_days=", false: ","}[i == 0] + fmt.Sprint(d)
		}
		t.Run(c.key+fmt.Sprint(c.days), func(t *testing.T) {
			page, _ := e.Must(200, "GET", "/v1/open-items?legal_entity_id="+e.de.String()+"&partner_id="+north.String()+query, "", "")
			listed := map[string]string{}
			for _, r := range page["aging"].([]any) {
				m := r.(map[string]any)
				listed[fmt.Sprintf("%v %v %v", m["bucket"], m["days_from"], m["days_to"])] = fmt.Sprintf("%v %v", m["item_count"],
					m["amount_lc"].(map[string]any)["amount"])
			}
			days, err := ledger.AgingDays(c.days)
			if err != nil {
				t.Fatal(err)
			}
			counts, sums := map[string]int{}, map[string]money.Minor{}
			for _, it := range items {
				// The list ages an item as posted: without terms, without a due date.
				name, from, to := ledger.AgingRange(ledger.AgingBucket(it.DaysOverdue(date(c.key)), it.Terms != nil, days), days)
				key := name + " <nil> <nil>"
				if from != nil {
					key = fmt.Sprintf("%s %d <nil>", name, *from)
					if to != nil {
						key = fmt.Sprintf("%s %d %d", name, *from, *to)
					}
				}
				counts[key], sums[key] = counts[key]+1, sums[key]+it.AmountLC
			}
			read := map[string]string{}
			for key, n := range counts {
				read[key] = fmt.Sprintf("%d %s", n, sums[key].Plain(2))
			}
			if !maps.Equal(read, listed) {
				t.Errorf("buckets: got %v from AgingBucket, want the list's %v", read, listed)
			}
		})
	}
	// The item without terms is dated 4 March: due then for the reader.
	without := items[3]
	if due := without.NetDueDate(); without.Terms != nil || due != date("2026-03-04") {
		t.Fatalf("the item without terms: got terms %+v, due %s; want none, due on its document date 2026-03-04", without.Terms, due.Format("2006-01-02"))
	}
	for _, c := range []struct {
		name, key string
		bucket    int
	}{
		{"on its document date", "2026-03-04", 0}, {"the day after", "2026-03-05", 1}, {"30 days after", "2026-04-03", 1},
		{"31 days after", "2026-04-04", 2}, {"91 days after", "2026-06-03", 4},
	} {
		t.Run("without terms, "+c.name, func(t *testing.T) {
			if got := ledger.AgingBucket(without.DaysOverdue(date(c.key)), true, []int32{30, 60, 90}); got != c.bucket {
				t.Errorf("bucket on %s: got %d, want %d", c.key, got, c.bucket)
			}
		})
	}
}
