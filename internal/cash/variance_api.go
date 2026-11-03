package cash

import (
	"context"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
)

// GetLiquidityForecastVariance implements GET /v1/liquidity-forecast-variance.
func (e *Endpoints) GetLiquidityForecastVariance(ctx context.Context, params gen.GetLiquidityForecastVarianceParams) (gen.GetLiquidityForecastVarianceRes, error) {
	v, err := e.Variance(ctx, VarianceQuery{LegalEntity: params.LegalEntityID, From: params.From, To: params.To, Lag: int(params.Lag.Or(DefaultLag)),
		Grain: params.Grain.Value})
	if err != nil {
		return nil, err
	}
	s := v.Snapshot
	out := gen.LiquidityForecastVariance{LegalEntityID: v.LegalEntity, CompanyCode: v.CompanyCode, ReadDate: v.ReadDate, TimeZone: v.TimeZone, From: v.From,
		To: v.To, Lag: int32(v.Lag), Grain: v.Grain, Buckets: make([]gen.LiquidityForecastBucket, len(v.Buckets)),
		Snapshot: gen.LiquidityForecastVarianceSnapshot{ID: s.ID, Kind: s.Kind, Name: apitypes.OptString(s.Name), SnapshotDate: s.Date, TakenAt: s.TakenAt,
			HorizonEnd: s.HorizonEnd, DocumentLevelUntil: s.DocumentLevelUntil(), ConfigVersion: s.ConfigVersion},
		Currencies: make([]gen.LiquidityForecastVarianceCurrency, len(v.Currencies))}
	for i, b := range v.Buckets {
		out.Buckets[i] = gen.LiquidityForecastBucket{Grain: b.Grain, Start: b.Start, End: b.End}
	}
	if b := v.Beyond; b != nil {
		out.Beyond.SetTo(gen.LiquidityForecastVariancePeriod{From: b.From, To: b.To})
	}
	for i, c := range v.Currencies {
		amount, beyond := amounts(c.Currency, c.Exponent), v.Beyond != nil
		out.Currencies[i] = gen.LiquidityForecastVarianceCurrency{Currency: c.Currency, Buckets: cellsToAPI(c.Buckets, amount), Total: cellToAPI(c.Total, amount),
			Beyond: optFigures(beyond, c.Beyond, amount), Items: make([]gen.LiquidityForecastVarianceItem, len(c.Items))}
		for j, r := range c.Items {
			out.Currencies[i].Items[j] = gen.LiquidityForecastVarianceItem{LiquidityItem: apitypes.OptText(r.Item), Name: apitypes.OptText(r.Name),
				Parent: apitypes.OptText(r.Parent), Level: int32(r.Level), Buckets: cellsToAPI(r.Buckets, amount), Total: cellToAPI(r.Total, amount),
				Beyond: optFigures(beyond, r.Beyond, amount)}
		}
	}
	return &out, nil
}

// cellsToAPI shows the cells of a row.
func cellsToAPI(cs []VarianceCell, amount func(money.Minor) gen.Money) []gen.LiquidityForecastVarianceCell {
	out := make([]gen.LiquidityForecastVarianceCell, len(cs))
	for i, c := range cs {
		out[i] = cellToAPI(c, amount)
	}
	return out
}

// cellToAPI shows a cell: its figures, the variance's size in percent of
// the forecast's, and its attribution.
func cellToAPI(c VarianceCell, amount func(money.Minor) gen.Money) gen.LiquidityForecastVarianceCell {
	return gen.LiquidityForecastVarianceCell{Forecast: amount(c.Forecast), Actual: amount(c.Actual), Variance: amount(c.Variance),
		VariancePercent: apitypes.AbsPercent(c.Variance, c.Forecast), Attribution: gen.LiquidityForecastVarianceAttribution{Timing: amount(c.Timing),
			Amount: amount(c.Amount), New: amount(c.New), Missing: amount(c.Missing), Unclassified: amount(c.Unclassified)}}
}

// optFigures shows the figures beyond the document level, when the
// variance compares any.
func optFigures(beyond bool, f Figures, amount func(money.Minor) gen.Money) gen.OptLiquidityForecastVarianceFigures {
	if !beyond {
		return gen.OptLiquidityForecastVarianceFigures{}
	}
	return gen.NewOptLiquidityForecastVarianceFigures(gen.LiquidityForecastVarianceFigures{Forecast: amount(f.Forecast), Actual: amount(f.Actual),
		Variance: amount(f.Variance), VariancePercent: apitypes.AbsPercent(f.Variance, f.Forecast)})
}
