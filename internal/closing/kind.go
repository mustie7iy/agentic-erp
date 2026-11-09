package closing

import "strings"

// closeKind is what tells a period close from a year close (ADR-0060 §8):
// the Go code of their tasks is shared, and only their tables, the periods
// they cover and the names their records use differ. A year close is
// stored apart from period closes, since a replica of the release before
// reads every period_close row and would fail on one without a period.
type closeKind struct {
	year   bool   // a year close: no period, its year's periods covered
	noun   string // "period close": what problems call it
	title  string // "Period close": the noun opening a sentence
	entity string // the audit records' entity, their actions' prefix and the events' aggregate type
	event  string // the event each change publishes
	idKey  string // the event's key of the close
	path   string // the close's resource, its id appended
	// sql names the kind's tables and columns in a statement both kinds
	// run: {close}, {task} and {ledger} its tables, {period} its period
	// (none for a year close: 0), {end} the last day its tasks' due dates
	// count from, and {signed} the latest sign-off of a close c signed off.
	sql *strings.Replacer
}

var (
	periodCloses = &closeKind{noun: "period close", title: "Period close", entity: "closing.period_close", event: "period_close_changed",
		idKey: "period_close_id", path: "/v1/period-closes/", sql: strings.NewReplacer("{close}", "closing.period_close", "{task}", "closing.close_task",
			"{ledger}", "closing.close_ledger", "{period}", "c.fiscal_period", "{end}", "c.period_end",
			"{signed}", `(SELECT s.signed_at, s.signed_by FROM closing.sign_off s WHERE s.tenant_id = c.tenant_id AND s.close_id = c.id
				AND c.status = 'signed_off' ORDER BY s.signed_at DESC, s.id DESC LIMIT 1)`)}
	// The year close's sign-off is T64m6's: until then it has none.
	yearCloses = &closeKind{year: true, noun: "year close", title: "Year close", entity: "closing.year_close", event: "year_close_changed",
		idKey: "year_close_id", path: "/v1/year-closes/", sql: strings.NewReplacer("{close}", "closing.year_close", "{task}", "closing.year_close_task",
			"{ledger}", "closing.year_close_ledger", "{period}", "0::smallint", "{end}", "c.year_end",
			"{signed}", `(SELECT NULL::timestamptz AS signed_at, NULL::uuid AS signed_by)`)}
)

// q is a statement both kinds run, with the kind's tables and columns.
func (k *closeKind) q(stmt string) string { return k.sql.Replace(stmt) }
