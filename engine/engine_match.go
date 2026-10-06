package engine

import (
	"github.com/lechandonga/backtest-matching-engine-go/event"
)

// fillOneLocked 给单边订单记一笔成交（行情驱动挂单成交路径，对手是市场）。
func (e *Engine) fillOneLocked(o *Order, px float64, qty, seq int64, res *Result) {
	qty = min64(qty, o.RemainingQty())
	if qty <= 0 {
		return
	}
	if o.Side == event.Buy {
		ref := e.consumeBuyReserveLocked(o, qty)
		if err := e.pf.FillBuy(o.Symbol, qty, px, ref); err != nil {
			panic("engine: portfolio rejected fill: " + err.Error())
		}
	} else {
		if err := e.pf.FillSell(o.Symbol, qty, px); err != nil {
			panic("engine: portfolio rejected fill: " + err.Error())
		}
	}
	o.applyFill(qty, px)
	res.Fills = append(res.Fills, e.makeFill(res.EventID, o, px, qty, seq))

	if o.Status.IsTerminal() {
		return
	}
	// 行情只吃掉挂单一部分时，挂单继续保留在簿中；DAY 单不到日终不失效。
}

// fillPairLocked 吃单与一张挂单成交：两边同时记账，保证账户恒等式不被破坏。
func (e *Engine) fillPairLocked(taker, maker *Order, px float64, qty, seq int64, res *Result) {
	qty = min64(qty, min64(taker.RemainingQty(), maker.RemainingQty()))
	if qty <= 0 {
		return
	}
	// 同一账户双边成交：价款净额为 0、费用两腿照扣、总股数不变，
	// 必须走原子自撮记账，避免均价在两腿之间被改写。
	var buyer *Order
	if taker.Side == event.Buy {
		buyer = taker
	} else {
		buyer = maker
	}
	buyerRef := e.consumeBuyReserveLocked(buyer, qty)
	if err := e.pf.FillSelfCross(taker.Symbol, qty, px, buyerRef*float64(qty)); err != nil {
		panic("engine: portfolio rejected self-cross fill: " + err.Error())
	}
	taker.applyFill(qty, px)
	maker.applyFill(qty, px)
	res.Fills = append(res.Fills,
		e.makeFill(res.EventID, taker, px, qty, seq),
		e.makeFill(res.EventID, maker, px, qty, seq))
}

func (e *Engine) makeFill(eventID string, o *Order, px float64, qty, seq int64) Fill {
	return Fill{
		EventID: eventID, OrderID: o.ID, Symbol: o.Symbol, Side: o.Side,
		Price: px, Qty: qty, Fee: px * float64(qty) * e.pf.FeeRate(), Seq: seq,
	}
}

// runMarketLocked 市价单（含止损触发）：立即吃对手簿；FOK 原子预检，IOC 剩余撤销。
func (e *Engine) runMarketLocked(o *Order, _ float64, seq int64, res *Result) {
	opp := event.Sell
	if o.Side == event.Sell {
		opp = event.Buy
	}
	book := e.bookFor(o.Symbol, opp)

	if o.TIF == event.TFOK {
		var avail int64
		for _, b := range book {
			avail += b.RemainingQty()
		}
		if avail < o.Qty {
			// FOK 不满足：整单一点不成交，冻结全部释放。
			e.terminateUnfilledLocked(o, StatusCanceled, "", "FOK cannot be fully filled", res)
			return
		}
	}

	if len(book) == 0 {
		e.terminateUnfilledLocked(o, StatusRejected, RejectNoMarketData,
			"market order without any counterparty", res)
		return
	}

	book = e.consumeBookLocked(o, book, func(_, _ float64) bool { return true }, seq, res)
	e.setBook(o.Symbol, opp, pruneTerminal(book))

	if o.RemainingQty() > 0 {
		switch o.TIF {
		case event.TFOK:
			panic("engine: FOK market order left residual after availability check")
		case event.TIFO:
			e.cancelRemainderLocked(o, res)
		default:
			// DAY/GTC 市价单不挂簿：部分成交后剩余立即撤销；一笔未成按无流动性拒单。
			if o.FilledQty == 0 {
				e.terminateUnfilledLocked(o, StatusRejected, RejectNoLiquidity,
					"market order not filled", res)
			} else {
				e.cancelRemainderLocked(o, res)
			}
		}
	}
}

// runLimitLocked 限价单（含止损限价触发）：立即穿越，FOK 原子预检，IOC 剩余撤销，其余挂簿。
func (e *Engine) runLimitLocked(o *Order, limit float64, seq int64, res *Result) {
	opp := event.Sell
	hit := func(oppLimit float64) bool { return oppLimit <= limit }
	if o.Side == event.Sell {
		opp = event.Buy
		hit = func(oppLimit float64) bool { return oppLimit >= limit }
	}
	book := e.bookFor(o.Symbol, opp)

	if o.TIF == event.TFOK {
		var avail int64
		for _, b := range book {
			if !hit(b.LimitPrice) {
				break
			}
			avail += b.RemainingQty()
		}
		// 对手簿之外，当前行情对手价可成交的数量也计入可用量（回测盘口深度按
		// 挂单簿 + 提交时可见的盘口报价估算：此处按"报价可满足剩余全部"处理，
		// 即只要限价已穿越对手价，市场侧流动性视为可吃）。
		if px, ok := e.oppositeQuoteLocked(o.Symbol, o.Side); ok && hit(px) {
			avail = o.Qty
		}
		if avail < o.Qty {
			e.terminateUnfilledLocked(o, StatusCanceled, "", "FOK cannot be fully filled", res)
			return
		}
	}

	book = e.consumeBookLocked(o, book, func(oppLimit, _ float64) bool { return hit(oppLimit) }, seq, res)
	e.setBook(o.Symbol, opp, pruneTerminal(book))

	// 簿中不够吃但限价已穿越当前对手价：剩余按对手价对市场成交。
	if o.RemainingQty() > 0 {
		if px, ok := e.oppositeQuoteLocked(o.Symbol, o.Side); ok && hit(px) {
			e.fillOneLocked(o, px, o.RemainingQty(), seq, res)
		}
	}

	if o.RemainingQty() > 0 {
		switch o.TIF {
		case event.TFOK:
			panic("engine: FOK limit order left residual after availability check")
		case event.TIFO:
			e.cancelRemainderLocked(o, res)
		default:
			e.insertBookLocked(o)
		}
	}
}

// oppositeQuoteLocked 返回当前盘口对手价（买单看卖一，卖单看买一）。
func (e *Engine) oppositeQuoteLocked(symbol string, side event.Side) (float64, bool) {
	if side == event.Buy {
		px, ok := e.askPrice[symbol]
		return px, ok
	}
	px, ok := e.bidPrice[symbol]
	return px, ok
}

// consumeBookLocked 用 taker 依次吃对手簿，返回（可能被吃尽/部分吃掉后的）簿。
func (e *Engine) consumeBookLocked(taker *Order, book []*Order,
	hit func(oppLimit, takerLimit float64) bool, seq int64, res *Result) []*Order {
	for len(book) > 0 && taker.RemainingQty() > 0 {
		top := book[0]
		if !hit(top.LimitPrice, taker.LimitPrice) {
			break
		}
		qty := min64(taker.RemainingQty(), top.RemainingQty())
		e.fillPairLocked(taker, top, top.LimitPrice, qty, seq, res)
		if top.Status.IsTerminal() {
			book = book[1:]
			continue
		}
		break
	}
	return book
}

func pruneTerminal(book []*Order) []*Order {
	out := make([]*Order, 0, len(book))
	for _, b := range book {
		if !b.Status.IsTerminal() {
			out = append(out, b)
		}
	}
	return out
}

// terminateUnfilledLocked 整单一笔未成交即终止：释放全部冻结，不产生任何成交。
func (e *Engine) terminateUnfilledLocked(o *Order, st OrderStatus, reason RejectReason, detail string, res *Result) {
	if o.FilledQty != 0 {
		panic("engine: terminateUnfilled on partially filled order")
	}
	if o.Status != StatusWaitingTrigger {
		e.releaseReserveLocked(o, o.Qty)
	}
	o.Status = st
	o.RejectReason = reason
	if st == StatusRejected {
		res.Reject = reject(o.ID, reason, detail)
	}
}

// cancelRemainderLocked 撤销剩余并释放对应冻结（允许已有部分成交）。
func (e *Engine) cancelRemainderLocked(o *Order, res *Result) {
	rem := o.RemainingQty()
	if rem <= 0 {
		return
	}
	e.releaseReserveLocked(o, rem)
	o.Status = StatusCanceled
}
