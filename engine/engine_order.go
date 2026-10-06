package engine

import (
	"sort"

	"github.com/lechandonga/backtest-matching-engine-go/event"
)

// applySubmitLocked 处理委托提交。
func (e *Engine) applySubmitLocked(ev event.Event, res *Result) {
	if err := validateSubmit(ev); err != nil {
		res.OrderID = ev.OrderID
		res.Reject = err
		return
	}
	if _, dup := e.orders[ev.OrderID]; dup {
		res.OrderID = ev.OrderID
		res.Reject = reject(ev.OrderID, RejectDuplicateOrderID, "order id already exists")
		return
	}

	tif := ev.TIF
	if tif == event.TIFUnset {
		tif = event.TGTC
	}

	o := &Order{
		ID:          ev.OrderID,
		Symbol:      ev.Symbol,
		Side:        ev.Side,
		Type:        ev.Type,
		LimitPrice:  ev.Price,
		Qty:         ev.Qty,
		Seq:         ev.Seq,
		TIF:         tif,
		SubmitDay:   e.currentDay,
		StopPrice:   ev.StopPrice,
		TrailPct:    ev.TrailPct,
		TrailAmount: ev.TrailAmount,
	}
	e.orders[o.ID] = o
	e.byEventID[ev.EventID] = o.ID
	res.OrderID = o.ID

	if isTriggerType(ev.Type) {
		// 触发前不冻结资金/持仓、不参与撮合。
		o.Status = StatusWaitingTrigger
		if e.waiters[o.Symbol] == nil {
			e.waiters[o.Symbol] = make(map[string]*Order)
		}
		e.waiters[o.Symbol][o.ID] = o
		if ev.Type == event.TrailingStop {
			if px, ok := e.lastPx[o.Symbol]; ok {
				o.AnchorPrice = px
			}
		}
		return
	}

	ref := e.submitRefPriceLocked(ev)
	if err := e.reserveLocked(o, ref, false); err != nil {
		o.Status = StatusRejected
		o.RejectReason = err.Reason
		res.Reject = err
		return
	}
	o.RefPrice = ref
	o.Status = StatusNew

	switch ev.Type {
	case event.Market:
		e.runMarketLocked(o, ref, ev.Seq, res)
	case event.Limit:
		e.runLimitLocked(o, ev.Price, ev.Seq, res)
	}
}

func isTriggerType(t event.OrderType) bool {
	return t == event.Stop || t == event.StopLimit || t == event.TrailingStop
}

func validateSubmit(ev event.Event) *RejectError {
	if ev.OrderID == "" || ev.Symbol == "" {
		return reject(ev.OrderID, RejectInvalidEvent, "order id and symbol required")
	}
	if ev.Qty <= 0 {
		return reject(ev.OrderID, RejectInvalidQty, "qty must be positive")
	}
	if ev.Side != event.Buy && ev.Side != event.Sell {
		return reject(ev.OrderID, RejectInvalidSide, "side must be BUY or SELL")
	}
	switch ev.Type {
	case event.Market, event.Limit, event.Stop, event.StopLimit, event.TrailingStop:
	default:
		return reject(ev.OrderID, RejectInvalidType, "unknown order type")
	}
	if ev.Type == event.Limit && ev.Price <= 0 {
		return reject(ev.OrderID, RejectInvalidLimitPrice, "limit price must be positive")
	}
	tif := ev.TIF
	if tif == event.TIFUnset {
		tif = event.TGTC
	}
	if tif < event.TIODay || tif > event.TGTC {
		return reject(ev.OrderID, RejectInvalidTIF, "unknown time in force")
	}
	if isTriggerType(ev.Type) {
		switch ev.Type {
		case event.Stop, event.StopLimit:
			if ev.StopPrice <= 0 {
				return reject(ev.OrderID, RejectInvalidStopPrice, "stop price must be positive")
			}
			if ev.Type == event.StopLimit && ev.Price <= 0 {
				return reject(ev.OrderID, RejectInvalidLimitPrice, "stop-limit price must be positive")
			}
		case event.TrailingStop:
			if (ev.TrailPct <= 0 && ev.TrailAmount <= 0) ||
				(ev.TrailPct > 0 && ev.TrailAmount > 0) {
				return reject(ev.OrderID, RejectInvalidStopPrice,
					"exactly one of trail pct/amount must be positive")
			}
		}
	}
	return nil
}

// submitRefPriceLocked 提交时冻结资金参考价：限价用限价；市价用对手价；无行情拒绝。
func (e *Engine) submitRefPriceLocked(ev event.Event) float64 {
	if ev.Type == event.Limit {
		return ev.Price
	}
	if ev.Side == event.Buy {
		if px, ok := e.askPrice[ev.Symbol]; ok {
			return px
		}
	} else if px, ok := e.bidPrice[ev.Symbol]; ok {
		return px
	}
	return e.lastPx[ev.Symbol]
}

// reserveLocked 冻结买入资金或卖出持仓。trigger=true 使用触发专用拒单原因。
func (e *Engine) reserveLocked(o *Order, ref float64, trigger bool) *RejectError {
	if o.Side == event.Buy {
		if ref <= 0 {
			return reject(o.ID, RejectNoMarketData, "no reference price to reserve cash")
		}
		if err := e.pf.ReserveBuy(o.Symbol, o.Qty, ref); err != nil {
			reason := RejectInsufficientCash
			if trigger {
				reason = RejectTriggerNoCash
			}
			return reject(o.ID, reason, err.Error())
		}
		o.reserves = append(o.reserves, reserveSlice{qty: o.Qty, refPrice: ref})
		return nil
	}
	if err := e.pf.ReserveSell(o.Symbol, o.Qty); err != nil {
		reason := RejectInsufficientPos
		if trigger {
			reason = RejectTriggerNoPos
		}
		return reject(o.ID, reason, err.Error())
	}
	return nil
}

// applyCancelLocked 撤单：终态单（含已过期、已触发并结束）按老规矩拒绝。
func (e *Engine) applyCancelLocked(ev event.Event, res *Result) {
	o := e.orders[ev.OrderID]
	if o == nil {
		res.Reject = reject(ev.OrderID, RejectUnknownOrder, "order not found")
		return
	}
	res.OrderID = o.ID
	if o.Status == StatusExpired {
		res.Reject = reject(o.ID, RejectOrderExpired, "order already expired")
		return
	}
	if o.Status.IsTerminal() {
		res.Reject = reject(o.ID, RejectTerminalOrder, "order already finished")
		return
	}
	if !legalCancelFrom[o.Status] {
		res.Reject = reject(o.ID, RejectTerminalOrder, "status not cancellable: "+o.Status.String())
		return
	}

	cancelQty := o.RemainingQty()
	if ev.Qty > 0 {
		if ev.Qty > cancelQty {
			res.Reject = reject(o.ID, RejectCancelExceedQty, "cancel qty exceeds remaining")
			return
		}
		cancelQty = ev.Qty
	}

	fullCancel := cancelQty == o.Qty-o.FilledQty
	if o.Status == StatusWaitingTrigger {
		if fullCancel {
			delete(e.waiters[o.Symbol], o.ID)
		}
	} else {
		e.removeFromBookLocked(o, cancelQty)
		e.releaseReserveLocked(o, cancelQty)
	}

	// 部分撤单：削减委托总量，释放对应冻结，订单继续保持可成交/待触发。
	if cancelQty < o.Qty-o.FilledQty {
		o.Qty -= cancelQty
		return
	}

	// 全部剩余撤销：进入终态（允许带已成交部分）。
	o.Status = StatusCanceled
}

// insertBookLocked 价格优先、同价按 Seq 时间优先挂簿。
func (e *Engine) insertBookLocked(o *Order) {
	side := event.Buy
	if o.Side == event.Sell {
		side = event.Sell
	}
	book := e.bookFor(o.Symbol, side)
	pos := sort.Search(len(book), func(i int) bool {
		if side == event.Buy {
			if book[i].LimitPrice != o.LimitPrice {
				return book[i].LimitPrice < o.LimitPrice
			}
		} else {
			if book[i].LimitPrice != o.LimitPrice {
				return book[i].LimitPrice > o.LimitPrice
			}
		}
		return book[i].Seq > o.Seq
	})
	book = append(book, nil)
	copy(book[pos+1:], book[pos:])
	book[pos] = o
	e.setBook(o.Symbol, side, book)
}

func (e *Engine) removeFromBookLocked(o *Order, qty int64) {
	side := event.Buy
	if o.Side == event.Sell {
		side = event.Sell
	}
	book := e.bookFor(o.Symbol, side)
	for i, b := range book {
		if b == o {
			if qty >= o.RemainingQty() {
				book = append(book[:i], book[i+1:]...)
			}
			e.setBook(o.Symbol, side, book)
			return
		}
	}
}

func (e *Engine) bookFor(symbol string, side event.Side) []*Order {
	if side == event.Buy {
		return e.bids[symbol]
	}
	return e.asks[symbol]
}

func (e *Engine) setBook(symbol string, side event.Side, book []*Order) {
	if side == event.Buy {
		e.bids[symbol] = book
	} else {
		e.asks[symbol] = book
	}
}

func (e *Engine) releaseReserveLocked(o *Order, qty int64) {
	if qty <= 0 {
		return
	}
	if o.Side == event.Buy {
		// 买单冻结可能分段（预留价不同）：从栈顶逐段核销。
		need := qty
		for need > 0 && len(o.reserves) > 0 {
			n := len(o.reserves) - 1
			s := &o.reserves[n]
			take := min64(need, s.qty)
			e.pf.ReleaseBuy(s.refPrice, take)
			s.qty -= take
			need -= take
			if s.qty == 0 {
				o.reserves = o.reserves[:n]
			}
		}
	} else {
		e.pf.ReleaseSell(o.Symbol, qty)
	}
}

// consumeBuyReserveLocked 成交时核销买单冻结分段，返回本批冻结参考价（用于 FillBuy 多退少补）。
func (e *Engine) consumeBuyReserveLocked(o *Order, qty int64) float64 {
	var weighted float64
	need := qty
	for need > 0 && len(o.reserves) > 0 {
		n := len(o.reserves) - 1
		s := &o.reserves[n]
		take := min64(need, s.qty)
		weighted += s.refPrice * float64(take)
		s.qty -= take
		need -= take
		if s.qty == 0 {
			o.reserves = o.reserves[:n]
		}
	}
	if need > 0 {
		panic("engine: buy fill exceeds reserved quantity")
	}
	return weighted / float64(qty)
}
