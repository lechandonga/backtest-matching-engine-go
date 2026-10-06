package engine

import (
	"sort"

	"github.com/lechandonga/backtest-matching-engine-go/event"
)

// applyQuoteLocked 处理行情快照：更新对手价、盯市、评估条件单、推动可成交限价单。
func (e *Engine) applyQuoteLocked(ev event.Event, res *Result) {
	if ev.BidPrice <= 0 || ev.AskPrice <= 0 || ev.BidPrice >= ev.AskPrice {
		res.Reject = reject(ev.OrderID, RejectInvalidPrice, "quote requires 0 < bid < ask")
		return
	}
	e.bidPrice[ev.Symbol] = ev.BidPrice
	e.askPrice[ev.Symbol] = ev.AskPrice
	e.lastPx[ev.Symbol] = (ev.BidPrice + ev.AskPrice) / 2
	e.pf.SetMark(ev.Symbol, e.lastPx[ev.Symbol])

	e.evaluateWaitersLocked(ev, res)
	e.matchBookAtQuoteLocked(ev, res)
}

// applyTradeLocked 处理成交流：更新最新价、评估条件单，并按成交价推动限价单簿。
func (e *Engine) applyTradeLocked(ev event.Event, res *Result) {
	if ev.TradePrice <= 0 || ev.TradeSize <= 0 {
		res.Reject = reject(ev.OrderID, RejectInvalidPrice, "trade requires positive price and size")
		return
	}
	e.lastPx[ev.Symbol] = ev.TradePrice
	if _, ok := e.bidPrice[ev.Symbol]; !ok {
		// 仅有成交流时用成交价同时充当两侧参考（回测常见数据形态）。
		e.bidPrice[ev.Symbol] = ev.TradePrice
		e.askPrice[ev.Symbol] = ev.TradePrice
	}
	e.pf.SetMark(ev.Symbol, ev.TradePrice)

	e.evaluateWaitersLocked(ev, res)

	// 价格穿越时挂单价单可成交：买挂单价 <= 成交价，卖挂单价 >= 成交价。
	remaining := ev.TradeSize
	for _, side := range []event.Side{event.Buy, event.Sell} {
		book := e.bookFor(ev.Symbol, side)
		for len(book) > 0 && remaining > 0 {
			top := book[0]
			// 挂买：成交价跌到买限价才成交（限价 >= 成交价）；
			// 挂卖：成交价涨到卖限价才成交（限价 <= 成交价）。
			priceable := (side == event.Buy && top.LimitPrice >= ev.TradePrice) ||
				(side == event.Sell && top.LimitPrice <= ev.TradePrice)
			if !priceable {
				break
			}
			qty := min64(top.RemainingQty(), remaining)
			e.fillOneLocked(top, ev.TradePrice, qty, ev.Seq, res)
			remaining -= qty
			if top.Status.IsTerminal() {
				book = book[1:]
			} else {
				break // 行情成交量用尽
			}
		}
		e.setBook(ev.Symbol, side, book)
	}
}

// matchBookAtQuoteLocked 在买一/卖一价上撮合挂单（限价被穿越时按对手价成交）。
func (e *Engine) matchBookAtQuoteLocked(ev event.Event, res *Result) {
	bid, ask := ev.BidPrice, ev.AskPrice
	// 买单簿与卖一撮合；卖单簿与买一撮合。
	// 买挂单：卖价跌到买限价以下（限价 >= 卖价）才成交。
	e.crossBookAtLocked(ev.Symbol, event.Buy, ask, ev.Seq, res, func(limit, market float64) bool { return limit >= market })
	// 卖挂单：买价涨到卖限价以上（限价 <= 买价）才成交。
	e.crossBookAtLocked(ev.Symbol, event.Sell, bid, ev.Seq, res, func(limit, market float64) bool { return limit <= market })
}

func (e *Engine) crossBookAtLocked(symbol string, side event.Side, px float64, seq int64, res *Result,
	hit func(limitPrice, marketPrice float64) bool) {
	book := e.bookFor(symbol, side)
	for len(book) > 0 {
		top := book[0]
		if !hit(top.LimitPrice, px) {
			break
		}
		e.fillOneLocked(top, px, top.RemainingQty(), seq, res)
		if top.Status.IsTerminal() {
			book = book[1:]
			continue
		}
		break
	}
	e.setBook(symbol, side, book)
}

// evaluateWaitersLocked 对该标的所有待触发委托评估一次。确定性顺序：按 (Seq, OrderID)。
func (e *Engine) evaluateWaitersLocked(ev event.Event, res *Result) {
	w := e.waiters[ev.Symbol]
	if len(w) == 0 {
		return
	}
	keys := make([]string, 0, len(w))
	for id := range w {
		keys = append(keys, id)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := w[keys[i]], w[keys[j]]
		if a.Seq != b.Seq {
			return a.Seq < b.Seq
		}
		return a.ID < b.ID
	})
	for _, id := range keys {
		o, ok := w[id]
		if !ok {
			continue
		}
		// 触发判定价：买入止损看卖价（向上突破要买入），卖出止损看买价。
		// 成交流事件没有独立买卖价，使用成交价。
		px := e.lastPx[ev.Symbol]
		if ev.Kind == event.KindQuote {
			if o.Side == event.Buy {
				px = ev.AskPrice
			} else {
				px = ev.BidPrice
			}
		}
		if e.updateTrailAndCheckLocked(o, px) || e.checkStopLocked(o, px) {
			e.fireTriggerLocked(o, px, ev, res)
		}
	}
}

// updateTrailAndCheckLocked 更新跟踪止损锚点并判定回撤触发。
// 锚点只朝有利方向走；返回 true 表示回撤达到阈值。
func (e *Engine) updateTrailAndCheckLocked(o *Order, px float64) bool {
	if o.Type != event.TrailingStop {
		return false
	}
	if o.AnchorPrice == 0 {
		o.AnchorPrice = px
		return false
	}
	switch o.Side {
	case event.Buy:
		// 买入止损：价格下行有利，锚点只降不升。
		if px < o.AnchorPrice {
			o.AnchorPrice = px
		}
		return px >= o.AnchorPrice+e.trailGap(o)
	case event.Sell:
		// 卖出止损：价格上行有利，锚点只升不降。
		if px > o.AnchorPrice {
			o.AnchorPrice = px
		}
		return px <= o.AnchorPrice-e.trailGap(o)
	}
	return false
}

func (e *Engine) trailGap(o *Order) float64 {
	if o.TrailAmount > 0 {
		return o.TrailAmount
	}
	if o.AnchorPrice > 0 && o.TrailPct > 0 {
		return o.AnchorPrice * o.TrailPct
	}
	return 0
}

func (e *Engine) checkStopLocked(o *Order, px float64) bool {
	switch o.Type {
	case event.Stop, event.StopLimit:
		if o.Side == event.Buy {
			return px >= o.StopPrice
		}
		return px <= o.StopPrice
	}
	return false
}

// fireTriggerLocked 触发一张条件单：只触发一次；触发瞬间资金/持仓不足则拒单，
// 不产生任何冻结。触发成功后进入普通委托流程。
func (e *Engine) fireTriggerLocked(o *Order, px float64, ev event.Event, res *Result) {
	delete(e.waiters[o.Symbol], o.ID)
	o.TriggerPrice = px
	o.TriggeredSeq = ev.Seq

	effType := event.Market
	limit := 0.0
	if o.Type == event.StopLimit {
		effType = event.Limit
		limit = o.LimitPrice
	}

	ref := px
	if effType == event.Limit {
		ref = limit
	}
	if err := e.reserveLocked(o, ref, true); err != nil {
		o.Status = StatusRejected
		o.RejectReason = err.Reason
		res.Reject = err
		return
	}
	o.RefPrice = ref
	o.Status = StatusNew

	// 触发后走与普通委托完全相同的撮合路径。
	if effType == event.Market {
		e.runMarketLocked(o, px, ev.Seq, res)
	} else {
		e.runLimitLocked(o, limit, ev.Seq, res)
	}
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
