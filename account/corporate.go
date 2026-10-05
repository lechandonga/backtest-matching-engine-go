package account

// Split 拆股/合股：数量乘 ratio、成本价除 ratio，并同步卖单预留。
// 不改变持仓市值，因此不影响权益，只改变数量口径。
func (a *Account) Split(refID, symbol string, ratio float64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if p, ok := a.positions[symbol]; ok && p.qty != 0 {
		p.qty *= ratio
		p.avgCost /= ratio
	}
	for _, r := range a.reserves {
		if r.symbol == symbol && r.kind == reserveSell {
			r.amount *= ratio
		}
	}
	a.appendLocked(LedgerEntry{Kind: "split", RefID: refID, Symbol: symbol, Qty: ratio, Price: ratio})
}

// Dividend 每股派现：现金增加并计入公司行为调整（不计入已实现盈亏），返回派现总额。
func (a *Account) Dividend(refID, symbol string, perShare float64) float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	qty := 0.0
	if p := a.positions[symbol]; p != nil {
		qty = p.qty
	}
	cash := qty * perShare
	a.cash += cash
	a.corporateAdj += cash
	a.appendLocked(LedgerEntry{Kind: "dividend", RefID: refID, Symbol: symbol,
		Qty: qty, Price: perShare, Amount: cash})
	return cash
}
