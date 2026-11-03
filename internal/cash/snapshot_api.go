package cash

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
)

// TakeCashSnapshot implements POST /v1/cash-snapshots.
func (e *Endpoints) TakeCashSnapshot(ctx context.Context, req *gen.CashSnapshotCreate, _ gen.TakeCashSnapshotParams) (gen.TakeCashSnapshotRes, error) {
	s, err := e.TakeSnapshot(ctx, req.LegalEntityID, req.Name)
	if err != nil {
		return nil, err
	}
	return &gen.CashSnapshotHeaders{Location: gen.NewOptString("/v1/cash-snapshots/" + s.ID.String()), Response: snapshotToAPI(s)}, nil
}

// GetCashSnapshot implements GET /v1/cash-snapshots/{id}.
func (e *Endpoints) GetCashSnapshot(ctx context.Context, params gen.GetCashSnapshotParams) (gen.GetCashSnapshotRes, error) {
	s, err := e.Snapshot(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	out := snapshotToAPI(s)
	return &out, nil
}

// ListCashSnapshots implements GET /v1/cash-snapshots.
func (e *Endpoints) ListCashSnapshots(ctx context.Context, params gen.ListCashSnapshotsParams) (gen.ListCashSnapshotsRes, error) {
	ss, next, err := e.Snapshots(ctx, SnapshotsQuery{LegalEntity: apitypes.Ptr[uuid.UUID](params.LegalEntityID), Kind: params.Kind.Value,
		From: apitypes.Ptr[time.Time](params.From), To: apitypes.Ptr[time.Time](params.To), Page: apitypes.Page(params.OrderBy, params.PageSize, params.PageToken)})
	if err != nil {
		return nil, err
	}
	out := gen.CashSnapshotPage{Items: make([]gen.CashSnapshot, len(ss)), NextPageToken: apitypes.OptText(next)}
	for i, s := range ss {
		out.Items[i] = snapshotToAPI(s)
	}
	return &out, nil
}

// ListCashSnapshotFlows implements GET /v1/cash-snapshots/{id}/flows.
func (e *Endpoints) ListCashSnapshotFlows(ctx context.Context, params gen.ListCashSnapshotFlowsParams) (gen.ListCashSnapshotFlowsRes, error) {
	size, err := pagination.Size(params.PageSize.Value, params.PageSize.Set)
	if err != nil {
		return nil, err
	}
	p, err := e.SnapshotLines(ctx, SnapshotLinesQuery{Snapshot: params.ID, Currency: params.Currency, Item: params.LiquidityItem.Value,
		Unclassified: params.Unclassified.Value, Source: params.Source.Value, From: apitypes.Ptr[time.Time](params.From), To: apitypes.Ptr[time.Time](params.To),
		PageSize: size, PageToken: params.PageToken.Value})
	if err != nil {
		return nil, err
	}
	amount := amounts(params.Currency, p.Exponent)
	out := gen.CashSnapshotFlowPage{SnapshotID: p.Snapshot.ID, LegalEntityID: p.Snapshot.LegalEntity, CompanyCode: p.Snapshot.CompanyCode, SnapshotDate: p.Snapshot.Date, Currency: params.Currency,
		LiquidityItem: params.LiquidityItem, Unclassified: params.Unclassified.Value, Total: amount(p.Total), Count: int32(p.Count),
		Items: make([]gen.CashSnapshotFlow, len(p.Lines)), NextPageToken: apitypes.OptText(p.Next)}
	for i, l := range p.Lines {
		f := gen.CashSnapshotFlow{LineNo: int32(l.LineNo), FlowNo: int32(l.FlowNo), ValueDate: l.Date, BucketEnd: apitypes.OptDate(l.End),
			HouseBankID: apitypes.OptUUID(l.Bank), Source: l.Source, Certainty: l.Certainty, Late: l.Late, LiquidityItem: apitypes.OptString(l.Item),
			Amount: amount(l.Amount), FlowCount: int32(l.Count), PaymentID: apitypes.OptUUID(l.Payment), HasDocument: l.HasDocument}
		if l.Bank != nil {
			f.HouseBankCode.SetTo(l.BankCode)
		}
		if s := l.Supplier; s != nil {
			f.Supplier.SetTo(gen.PartnerName{ID: s.ID, Number: s.Number, Name: s.Name})
		}
		if d := l.Document; d != nil {
			f.Document.SetTo(gen.CashSnapshotDocument{Kind: d.Kind, ID: d.ID, Number: apitypes.OptText(d.Number)})
		}
		out.Items[i] = f
	}
	return &out, nil
}

// snapshotToAPI shows a snapshot with its totals per currency.
func snapshotToAPI(s Snapshot) gen.CashSnapshot {
	out := gen.CashSnapshot{ID: s.ID, LegalEntityID: s.LegalEntity, CompanyCode: s.CompanyCode, Kind: s.Kind, Name: apitypes.OptString(s.Name), SnapshotDate: s.Date, TimeZone: s.TimeZone,
		HorizonEnd: s.HorizonEnd, DocumentLevelUntil: s.DocumentLevelUntil(), ConfigVersion: s.ConfigVersion, TakenAt: s.TakenAt,
		Currencies: make([]gen.CashSnapshotCurrency, len(s.Currencies))}
	for i, c := range s.Currencies {
		amount := amounts(c.Currency, c.Exponent)
		out.Currencies[i] = gen.CashSnapshotCurrency{Currency: c.Currency, Total: amount(c.Total), Blocked: amount(c.Blocked), LineCount: int32(c.Lines)}
	}
	return out
}
