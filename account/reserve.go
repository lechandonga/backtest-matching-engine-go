package account

import "fmt"

// PlaceBuy 为买单预留 qty*price + 预估费用。
func (a *Account) PlaceBuy(orderID, symbol string, qty, price float64, fee FeeModel) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.reserves[orderID]; ok {
		return fmt.Errorf("%w: %s", ErrDuplicateReservation, orderID)
	}
	need := qty*price + fee.Fee(qty*price, qty)
	if a.availableCashLocked() < need {
		return fmt.Errorf("%w: need %.6f, available %.6f",
			ErrInsufficientCash, need, a.availableCashLocked())
	}
	a.reserves[orderID] = &reservation{kind: reserveBuy, symbol: symbol, amount: need}
	a.appendLocked(LedgerEntry{Kind: "reserve", RefID: orderID, Symbol: symbol,
		Qty: qty, Price: price, Amount: -need})
	return nil
}

// PlaceSell 为卖单预留持仓（默认禁止卖空）。
func (a *Account) PlaceSell(orderID, symbol string, qty float64) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.reserves[orderID]; ok {
		return fmt.Errorf("%w: %s", ErrDuplicateReservation, orderID)
	}
	avail := a.availableQtyLocked(symbol)
	if avail < qty {
		return fmt.Errorf("%w: need %.6f of %s, available %.6f",
			ErrInsufficientPosition, qty, symbol, avail)
	}
	a.reserves[orderID] = &reservation{kind: reserveSell, symbol: symbol, amount: qty}
	a.appendLocked(LedgerEntry{Kind: "reserve", RefID: orderID, Symbol: symbol, Qty: qty})
	return nil
}

// AdjustBuyReservation 成交后把买单预留调整为剩余数量所需金额；remainingQty<=0 表示释放。
func (a *Account) AdjustBuyReservation(orderID, symbol string, remainingQty, price float64, fee FeeModel) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	r, ok := a.reserves[orderID]
	if !ok || r.kind != reserveBuy {
		return fmt.Errorf("%w: %s", ErrUnknownReservation, orderID)
	}
	old := r.amount
	if remainingQty <= 0 {
		delete(a.reserves, orderID)
		a.appendLocked(LedgerEntry{Kind: "release", RefID: orderID, Symbol: symbol, Amount: old})
		return nil
	}
	need := remainingQty*price + fee.Fee(remainingQty*price, remainingQty)
	r.amount = 0
	if a.availableCashLocked() < need {
		r.amount = old
		return fmt.Errorf("%w: need %.6f, available %.6f after partial fill",
			ErrInsufficientCash, need, a.availableCashLocked())
	}
	r.amount = need
	a.appendLocked(LedgerEntry{Kind: "reserve", RefID: orderID, Symbol: symbol,
		Qty: remainingQty, Price: price, Amount: -(need - old)})
	return nil
}

// AdjustSellReservation 成交后把卖单预留调整为剩余数量；remainingQty<=0 表示释放。
func (a *Account) AdjustSellReservation(orderID, symbol string, remainingQty float64) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	r, ok := a.reserves[orderID]
	if !ok || r.kind != reserveSell {
		return fmt.Errorf("%w: %s", ErrUnknownReservation, orderID)
	}
	if remainingQty <= 0 {
		delete(a.reserves, orderID)
	} else {
		r.amount = remainingQty
	}
	return nil
}

// Release 撤销时释放订单全部预留。
func (a *Account) Release(orderID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	r, ok := a.reserves[orderID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownReservation, orderID)
	}
	if r.kind == reserveBuy {
		a.appendLocked(LedgerEntry{Kind: "release", RefID: orderID, Symbol: r.symbol, Amount: r.amount})
	} else {
		a.appendLocked(LedgerEntry{Kind: "release", RefID: orderID, Symbol: r.symbol, Qty: r.amount})
	}
	delete(a.reserves, orderID)
	return nil
}

func (a *Account) availableCashLocked() float64 {
	locked := 0.0
	for _, r := range a.reserves {
		if r.kind == reserveBuy {
			locked += r.amount
		}
	}
	return a.cash - locked
}

func (a *Account) availableQtyLocked(symbol string) float64 {
	held := 0.0
	if p := a.positions[symbol]; p != nil {
		held = p.qty
	}
	locked := 0.0
	for _, r := range a.reserves {
		if r.kind == reserveSell && r.symbol == symbol {
			locked += r.amount
		}
	}
	return held - locked
}
