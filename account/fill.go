package account

import "fmt"

// BuyFilled 记账一笔买入（按 FillID 幂等）。预留由引擎另行调整/释放。
func (a *Account) BuyFilled(f Fill) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if dup, err := a.dedupeLocked(f); dup {
		return err
	}
	p := a.positions[f.Symbol]
	if p == nil {
		p = &position{}
		a.positions[f.Symbol] = p
	}
	notional := f.Qty * f.Price
	if p.qty == 0 {
		p.avgCost = f.Price
	} else {
		p.avgCost = (p.qty*p.avgCost + notional) / (p.qty + f.Qty)
	}
	p.qty += f.Qty
	a.cash -= notional + f.Fee
	a.fees += f.Fee
	a.appendLocked(LedgerEntry{Kind: "fill_buy", RefID: f.FillID, Symbol: f.Symbol,
		Qty: f.Qty, Price: f.Price, Amount: -(notional + f.Fee)})
	return nil
}

// SellFilled 记账一笔卖出（按 FillID 幂等），并实现已实现盈亏。
func (a *Account) SellFilled(f Fill) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if dup, err := a.dedupeLocked(f); dup {
		return err
	}
	p := a.positions[f.Symbol]
	held := 0.0
	if p != nil {
		held = p.qty
	}
	if held < f.Qty {
		return fmt.Errorf("%w: cannot sell %.6f of %s, held %.6f",
			ErrInsufficientPosition, f.Qty, f.Symbol, held)
	}
	notional := f.Qty * f.Price
	realized := (f.Price - p.avgCost) * f.Qty
	p.qty -= f.Qty
	if p.qty == 0 {
		p.avgCost = 0
	}
	a.realizedPnL += realized
	a.cash += notional - f.Fee
	a.fees += f.Fee
	a.appendLocked(LedgerEntry{Kind: "fill_sell", RefID: f.FillID, Symbol: f.Symbol,
		Qty: f.Qty, Price: f.Price, Amount: notional - f.Fee, Realized: realized})
	return nil
}

// dedupeLocked 返回 (true, nil) 表示命中已有成交（调用方应直接返回，不记账），
// (true, err) 表示同 ID 但内容冲突，(false, nil) 表示新成交并已登记。
func (a *Account) dedupeLocked(f Fill) (bool, error) {
	if old, ok := a.seenFills[f.FillID]; ok {
		if old != f {
			return true, fmt.Errorf("%w: %s", ErrDuplicateFill, f.FillID)
		}
		return true, nil
	}
	a.seenFills[f.FillID] = f
	return false, nil
}

func (a *Account) appendLocked(e LedgerEntry) {
	a.ledger = append(a.ledger, e)
}
