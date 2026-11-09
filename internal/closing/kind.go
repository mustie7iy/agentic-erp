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
	// count from, {signoff} its sign-offs' table, and {signed} the latest
	// sign-off of a close c signed off.
	// api TestProblemCodes reads the table a statement writes from its
	// text, so it does not see these: a problem registered on a
	// constraint of the closes', tasks' or ledgers' tables (none is but
	// the closes' *_once, which no statement here can break) needs the
	// test taught first.
	sql *strings.Replacer
}

var (
	periodCloses = &closeKind{noun: "period close", title: "Period close", entity: "closing.period_close", event: "period_close_changed",
		idKey: "period_close_id", path: "/v1/period-closes/", sql: strings.NewReplacer("{close}", "closing.period_close", "{task}", "closing.close_task",
			"{ledger}", "closing.close_ledger", "{period}", "c.fiscal_period", "{end}", "c.period_end", "{signoff}", "closing.sign_off",
			"{signed}", `(SELECT s.signed_at, s.signed_by FROM closing.sign_off s WHERE s.tenant_id = c.tenant_id AND s.close_id = c.id
				AND c.status = 'signed_off' ORDER BY s.signed_at DESC, s.id DESC LIMIT 1)`)}
	yearCloses = &closeKind{year: true, noun: "year close", title: "Year close", entity: "closing.year_close", event: "year_close_changed",
		idKey: "year_close_id", path: "/v1/year-closes/", sql: strings.NewReplacer("{close}", "closing.year_close", "{task}", "closing.year_close_task",
			"{ledger}", "closing.year_close_ledger", "{period}", "0::smallint", "{end}", "c.year_end", "{signoff}", "closing.year_sign_off",
			"{signed}", `(SELECT s.signed_at, s.signed_by FROM closing.year_sign_off s WHERE s.tenant_id = c.tenant_id AND s.close_id = c.id
				AND c.status = 'signed_off' ORDER BY s.signed_at DESC, s.id DESC LIMIT 1)`)}
)

// q is a statement both kinds run, with the kind's tables and columns.
func (k *closeKind) q(stmt string) string { return k.sql.Replace(stmt) }
