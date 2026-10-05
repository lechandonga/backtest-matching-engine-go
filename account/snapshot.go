package account

import (
	"math"
	"sort"
)

// Snapshot 按最新价 mark-to-market（marks 缺省的标的按成本价计）。
func (a *Account) Snapshot(marks map[string]float64) Snapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.snapshotLocked(marks)
}

// Reconcile 生成快照并强制校验权益恒等式：
//
//	Equity(=现金+市值) == InitialCash + RealizedPnL + UnrealizedPnL - Fees + CorporateAdjustments
func (a *Account) Reconcile(marks map[string]float64) (Snapshot, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.snapshotLocked(marks)
	want := s.InitialCash + s.RealizedPnL + s.UnrealizedPnL - s.Fees + s.CorporateAdjustments
	diff := want - s.Equity
	tol := 1e-9 * math.Max(1, math.Max(math.Abs(want), math.Abs(s.Equity)))
	if math.Abs(diff) > tol {
		return s, &ReconcileError{Snap: s, Want: want, Got: s.Equity, Diff: diff}
	}
	return s, nil
}

// Ledger 返回账务流水副本。
func (a *Account) Ledger() []LedgerEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]LedgerEntry, len(a.ledger))
	copy(out, a.ledger)
	return out
}

func (a *Account) snapshotLocked(marks map[string]float64) Snapshot {
	s := Snapshot{
		InitialCash:          a.initialCash,
		Cash:                 a.cash,
		RealizedPnL:          a.realizedPnL,
		Fees:                 a.fees,
		CorporateAdjustments: a.corporateAdj,
	}
	symbols := make([]string, 0, len(a.positions))
	for sym, p := range a.positions {
		if p.qty != 0 {
			symbols = append(symbols, sym)
		}
	}
	sort.Strings(symbols)
	for _, sym := range symbols {
		p := a.positions[sym]
		mark := p.avgCost
		if m, ok := marks[sym]; ok && m > 0 {
			mark = m
		}
		s.UnrealizedPnL += (mark - p.avgCost) * p.qty
		s.Equity += mark * p.qty
		s.Positions = append(s.Positions, Position{Symbol: sym, Qty: p.qty, AvgCost: p.avgCost})
	}
	s.Equity += a.cash
	return s
}
