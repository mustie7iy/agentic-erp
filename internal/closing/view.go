package closing

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/idref"
)

// companyDay is a close's company as its representation names it, with
// its date at the read, on which overdue tasks are judged.
type companyDay struct {
	Company
	day config.CompanyDay
}

// view is what a response names beside the rows it shows: their
// companies, each with its date at the read, and the people their tasks
// name.
type view struct {
	companies map[uuid.UUID]companyDay
	people    map[uuid.UUID]Person
}

// personRef names a principal missing from the identity context.
var personRef = idref.Kind{Name: "principal", Context: "identity"}

// view reads the companies of the legal entities and the people the tasks
// name, and the others named (signers). A legal entity the tenancy context
// does not hold, or a principal the identity context does not, is broken
// data and fails the response.
func (e *Endpoints) view(ctx context.Context, legalEntities []uuid.UUID, tasks []task, others ...uuid.UUID) (view, error) {
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return view{}, fmt.Errorf("closing: configuration: %w", err)
	}
	v := view{companies: map[uuid.UUID]companyDay{}}
	now := clock.Now(e.Now)
	for _, id := range legalEntities {
		if _, ok := v.companies[id]; ok {
			continue
		}
		co, ok, err := e.Companies.Company(ctx, id)
		if err == nil && !ok {
			err = fmt.Errorf("legal entity %s is not held by the tenancy context", id)
		}
		if err != nil {
			return view{}, fmt.Errorf("closing: company of %s: %w", id, err)
		}
		day, err := doc.CompanyDay(co.Code, now)
		if err != nil {
			return view{}, fmt.Errorf("closing: date of company %s: %w", co.Code, err)
		}
		v.companies[id] = companyDay{Company: co, day: day}
	}
	ids := append(idref.Of(tasks, func(t task) *uuid.UUID { return t.Assignee }), idref.Of(tasks, func(t task) *uuid.UUID { return t.CompletedBy })...)
	ids = append(append(ids, idref.Of(tasks, func(t task) *uuid.UUID { return t.CheckedBy })...), others...)
	if v.people, err = idref.Resolve(ctx, e.People.Named, ids, personRef); err != nil {
		return view{}, fmt.Errorf("closing: people of %d tasks: %w", len(tasks), err)
	}
	return v, nil
}

// taskToAPI shows a task with its close's company, overdue judged on the
// company's date at the read.
func (v view) taskToAPI(t task) gen.CloseTask {
	co := v.companies[t.LegalEntityID]
	out := gen.CloseTask{CloseID: t.CloseID, CloseVersion: t.CloseVersion, LegalEntityID: t.LegalEntityID, CompanyCode: co.Code, CompanyName: co.Name,
		FiscalYear: int32(t.FiscalYear), FiscalPeriod: int32(t.FiscalPeriod), Code: t.Code, Seq: int32(t.Seq), Name: t.Name, Kind: t.Kind,
		Check: apitypes.OptString(t.Check), DueOn: t.DueOn, DependsOn: t.DependsOn, BlockedBy: t.BlockedBy, AssigneeID: apitypes.OptUUID(t.Assignee),
		Status: t.Status, Overdue: t.Status == TaskOpen && t.DueOn.Before(co.day.Date), ReadDate: co.day.Date,
		CompletedBy: apitypes.OptUUID(t.CompletedBy), Note: apitypes.OptString(t.Note)}
	if t.Assignee != nil {
		out.AssigneeName.SetTo(v.people[*t.Assignee].Name)
	}
	if t.CompletedBy != nil {
		out.CompletedByName.SetTo(v.people[*t.CompletedBy].Name)
	}
	if t.CompletedAt != nil {
		out.CompletedAt.SetTo(*t.CompletedAt)
	}
	if t.CheckedAt != nil {
		// The database records a result whole (close_task_checked).
		out.CheckResult.SetTo(t.Found.toAPI(*t.Check, *t.CheckPassed, *t.CheckedAt, *t.CheckedBy, v.people[*t.CheckedBy].Name))
	}
	return out
}

// itemToAPI shows a close as a list does, its counts judged on its
// company's date at the read.
func (v view) itemToAPI(c closeRow) gen.PeriodCloseItem {
	co := v.companies[c.LegalEntityID]
	out := gen.PeriodCloseItem{ID: c.ID, LegalEntityID: c.LegalEntityID, CompanyCode: co.Code, CompanyName: co.Name, FiscalYear: int32(c.FiscalYear),
		FiscalPeriod: int32(c.FiscalPeriod), PeriodEnd: c.PeriodEnd, ConfigVersion: c.ConfigVersion, Status: c.Status, ReadDate: co.day.Date,
		TimeZone: co.day.Zone.String(), TasksTotal: c.Total, TasksDone: c.Done, TasksOpen: c.Total - c.Done, TasksOverdue: c.overdue(co.day.Date),
		Version: c.Version, CreatedAt: c.CreatedAt, CreatedBy: c.CreatedBy, UpdatedAt: c.UpdatedAt, UpdatedBy: c.UpdatedBy}
	if c.SignedBy != nil {
		// The latest sign-off, read with its close while it is signed off.
		out.SignedOffAt.SetTo(*c.SignedAt)
		out.SignedOffBy.SetTo(*c.SignedBy)
		out.SignedOffByName.SetTo(v.people[*c.SignedBy].Name)
	}
	return out
}

// signers are the people who signed off the closes, for their names.
func signers(cs ...closeRow) []uuid.UUID {
	return idref.Of(cs, func(c closeRow) *uuid.UUID { return c.SignedBy })
}

// signOffToAPI shows a sign-off of a close with the close's company and
// period.
func (v view) signOffToAPI(c closeRow, s signOff) gen.PeriodCloseSignOff {
	co := v.companies[c.LegalEntityID]
	out := gen.PeriodCloseSignOff{ID: s.ID, CloseID: s.CloseID, LegalEntityID: c.LegalEntityID, CompanyCode: co.Code, CompanyName: co.Name,
		FiscalYear: int32(c.FiscalYear), FiscalPeriod: int32(c.FiscalPeriod), SignedBy: s.SignedBy, SignedByName: v.people[s.SignedBy].Name,
		SignedAt: s.SignedAt, Note: apitypes.OptString(s.Note), Ledgers: make([]gen.PeriodCloseSignOffLedger, len(s.Ledgers))}
	for i, l := range s.Ledgers {
		out.Ledgers[i] = gen.PeriodCloseSignOffLedger{LedgerID: l.LedgerID, LedgerCode: l.LedgerCode, LedgerName: l.LedgerName, IsLeading: l.Leading,
			FiscalPeriodID: l.PeriodID, Locked: l.Locked}
	}
	return out
}

// closeToAPI shows a close with its ledgers, tasks and sign-offs.
func (v view) closeToAPI(c closeRow, ledgers []gen.CloseLedger, tasks []task, offs []signOff) gen.PeriodClose {
	i := v.itemToAPI(c)
	out := gen.PeriodClose{ID: i.ID, LegalEntityID: i.LegalEntityID, CompanyCode: i.CompanyCode, CompanyName: i.CompanyName, FiscalYear: i.FiscalYear,
		FiscalPeriod: i.FiscalPeriod, PeriodEnd: i.PeriodEnd, ConfigVersion: i.ConfigVersion, Status: i.Status, SignedOffAt: i.SignedOffAt,
		SignedOffBy: i.SignedOffBy, SignedOffByName: i.SignedOffByName, ReadDate: i.ReadDate, TimeZone: i.TimeZone, TasksTotal: i.TasksTotal,
		TasksDone: i.TasksDone, TasksOpen: i.TasksOpen, TasksOverdue: i.TasksOverdue, Ledgers: ledgers, Tasks: make([]gen.CloseTask, len(tasks)),
		SignOffs: make([]gen.PeriodCloseSignOff, len(offs)), Version: i.Version, CreatedAt: i.CreatedAt, CreatedBy: i.CreatedBy, UpdatedAt: i.UpdatedAt,
		UpdatedBy: i.UpdatedBy}
	for j, t := range tasks {
		out.Tasks[j] = v.taskToAPI(t)
	}
	for j, s := range offs {
		out.SignOffs[j] = v.signOffToAPI(c, s)
	}
	return out
}

// yearTaskToAPI shows a task of a year close as taskToAPI shows a period
// close's, without a period.
func (v view) yearTaskToAPI(t task) gen.YearCloseTask {
	o := v.taskToAPI(t)
	return gen.YearCloseTask{CloseID: o.CloseID, CloseVersion: o.CloseVersion, LegalEntityID: o.LegalEntityID, CompanyCode: o.CompanyCode,
		CompanyName: o.CompanyName, FiscalYear: o.FiscalYear, Code: o.Code, Seq: o.Seq, Name: o.Name, Kind: o.Kind, Check: o.Check, DueOn: o.DueOn,
		DependsOn: o.DependsOn, BlockedBy: o.BlockedBy, AssigneeID: o.AssigneeID, AssigneeName: o.AssigneeName, Status: o.Status, Overdue: o.Overdue,
		ReadDate: o.ReadDate, CompletedBy: o.CompletedBy, CompletedByName: o.CompletedByName, CompletedAt: o.CompletedAt, Note: o.Note}
}

// yearItemToAPI shows a year close as a list does, as itemToAPI shows a
// period close, its last day the year's.
func (v view) yearItemToAPI(c closeRow) gen.YearCloseItem {
	i := v.itemToAPI(c)
	return gen.YearCloseItem{ID: i.ID, LegalEntityID: i.LegalEntityID, CompanyCode: i.CompanyCode, CompanyName: i.CompanyName, FiscalYear: i.FiscalYear,
		YearEnd: i.PeriodEnd, ConfigVersion: i.ConfigVersion, Status: i.Status, ReadDate: i.ReadDate, TimeZone: i.TimeZone, TasksTotal: i.TasksTotal,
		TasksDone: i.TasksDone, TasksOpen: i.TasksOpen, TasksOverdue: i.TasksOverdue, Version: i.Version, CreatedAt: i.CreatedAt, CreatedBy: i.CreatedBy,
		UpdatedAt: i.UpdatedAt, UpdatedBy: i.UpdatedBy}
}

// yearCloseToAPI shows a year close with its ledgers' periods of the year
// and its tasks.
func (v view) yearCloseToAPI(c closeRow, covered []LedgerYear, tasks []task) gen.YearClose {
	i := v.yearItemToAPI(c)
	out := gen.YearClose{ID: i.ID, LegalEntityID: i.LegalEntityID, CompanyCode: i.CompanyCode, CompanyName: i.CompanyName, FiscalYear: i.FiscalYear,
		YearEnd: i.YearEnd, ConfigVersion: i.ConfigVersion, Status: i.Status, ReadDate: i.ReadDate, TimeZone: i.TimeZone, TasksTotal: i.TasksTotal,
		TasksDone: i.TasksDone, TasksOpen: i.TasksOpen, TasksOverdue: i.TasksOverdue, Ledgers: make([]gen.YearCloseLedger, len(covered)),
		Tasks: make([]gen.YearCloseTask, len(tasks)), Version: i.Version, CreatedAt: i.CreatedAt, CreatedBy: i.CreatedBy, UpdatedAt: i.UpdatedAt,
		UpdatedBy: i.UpdatedBy}
	for j, l := range covered {
		out.Ledgers[j] = gen.YearCloseLedger{LedgerID: l.LedgerID, LedgerCode: l.Code, LedgerName: l.Name, IsLeading: l.Leading,
			Periods: make([]gen.YearClosePeriod, len(l.Periods))}
		for n, p := range l.Periods {
			out.Ledgers[j].Periods[n] = gen.YearClosePeriod{FiscalPeriodID: p.ID, Period: int32(p.Number), IsSpecial: p.Special, Status: p.Status}
		}
	}
	for j, t := range tasks {
		out.Tasks[j] = v.yearTaskToAPI(t)
	}
	return out
}
