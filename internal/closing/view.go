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
// name. A legal entity the tenancy context does not hold, or a principal
// the identity context does not, is broken data and fails the response.
func (e *Endpoints) view(ctx context.Context, legalEntities []uuid.UUID, tasks []task) (view, error) {
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
	return out
}

// itemToAPI shows a close as a list does, its counts judged on its
// company's date at the read.
func (v view) itemToAPI(c periodClose) gen.PeriodCloseItem {
	co := v.companies[c.LegalEntityID]
	return gen.PeriodCloseItem{ID: c.ID, LegalEntityID: c.LegalEntityID, CompanyCode: co.Code, CompanyName: co.Name, FiscalYear: int32(c.FiscalYear),
		FiscalPeriod: int32(c.FiscalPeriod), PeriodEnd: c.PeriodEnd, ConfigVersion: c.ConfigVersion, Status: c.Status, ReadDate: co.day.Date,
		TimeZone: co.day.Zone.String(), TasksTotal: c.Total, TasksDone: c.Done, TasksOpen: c.Total - c.Done, TasksOverdue: c.overdue(co.day.Date),
		Version: c.Version, CreatedAt: c.CreatedAt, CreatedBy: c.CreatedBy, UpdatedAt: c.UpdatedAt, UpdatedBy: c.UpdatedBy}
}

// closeToAPI shows a close with its ledgers and tasks.
func (v view) closeToAPI(c periodClose, ledgers []gen.CloseLedger, tasks []task) gen.PeriodClose {
	i := v.itemToAPI(c)
	out := gen.PeriodClose{ID: i.ID, LegalEntityID: i.LegalEntityID, CompanyCode: i.CompanyCode, CompanyName: i.CompanyName, FiscalYear: i.FiscalYear,
		FiscalPeriod: i.FiscalPeriod, PeriodEnd: i.PeriodEnd, ConfigVersion: i.ConfigVersion, Status: i.Status, ReadDate: i.ReadDate, TimeZone: i.TimeZone,
		TasksTotal: i.TasksTotal, TasksDone: i.TasksDone, TasksOpen: i.TasksOpen, TasksOverdue: i.TasksOverdue, Ledgers: ledgers,
		Tasks: make([]gen.CloseTask, len(tasks)), Version: i.Version, CreatedAt: i.CreatedAt, CreatedBy: i.CreatedBy, UpdatedAt: i.UpdatedAt,
		UpdatedBy: i.UpdatedBy}
	for j, t := range tasks {
		out.Tasks[j] = v.taskToAPI(t)
	}
	return out
}
