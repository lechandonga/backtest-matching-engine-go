package engine

import (
	"sort"

	"github.com/lechandonga/backtest-matching-engine-go/event"
)

// crossingPrice 从行情事件中取出用于触发判定的确定性价格：
// 成交流优先于报价；报价事件没有历史成交流时用买一卖一中点。
func triggerPrice(symbol string, m *marketState) (float64, bool) {
	if m.hasTrade {
		return m.tradePrice, true
	}
	if m.hasQuote {
		return (m.bidPrice + m.askPrice) / 2, true
	}
	return 0, false
}

// evaluateTriggersLocked 评估某标的全部待触发委托。
// 触发只发生一次：Triggered 一旦置位，委托离开 waiting 集合，后续重复/乱序事件不可再次触发。
// 不同标的各自独立（waiting 按 symbol 分桶）。
func (e *Engine) evaluateTriggersLocked(symbol, eventID string, seq int64) {
	bucket := e.waiting[symbol]
	if len(bucket) == 0 {
		return
	}
	orders := make([]*Order, 0, len(bucket))
	for _, o := range bucket {
		orders = append(orders, o)
	}
	sort.Slice(orders, func(i, j int) bool {
		if orders[i].Seq != orders[j].Seq {
			return orders[i].Seq < orders[j].Seq
		}
		return orders[i].ID < orders[j].ID
	})

	m := e.market[symbol]
	price, ok := triggerPrice(symbol, m)
	if !ok {
		return
	}
	for _, o := range orders {
		if !o.IsWaitingTrigger() {
			continue
		}
		if e.updateTrailLocked(o, price) {
			e.fireTriggerLocked(o, price, eventID, seq)
			continue
		}
		if o.Type == event.StopMarket || o.Type == event.StopLimit {
			if hitsStop(o, price) {
				e.fireTriggerLocked(o, price, eventID, seq)
			}
		}
	}
}

func hitsStop(o *Order, price float64) bool {
	if o.Side == event.Buy {
		return price >= o.StopPrice
	}
	return price <= o.StopPrice
}

// updateTrailLocked 更新跟踪止损基准（只朝有利方向走），返回是否已回撤到触发阈值。
// 触发之后该函数不会再被调用（委托已离开 waiting），基准自然冻结。
func (e *Engine) updateTrailLocked(o *Order, price float64) bool {
	if o.Type != event.TrailingStop {
		return false
	}
	if o.Side == event.Buy {
		if o.TrailAnchor == 0 || price < o.TrailAnchor {
			o.TrailAnchor = price
		}
		if o.TrailPct > 0 {
			return price >= o.TrailAnchor*(1+o.TrailPct)
		}
		return price >= o.TrailAnchor+o.TrailDistance
	}
	// 卖出（本系统只支持多头，跟踪止损是常用止损形态）：基准跟最高价。
	if price > o.TrailAnchor {
		o.TrailAnchor = price
	}
	if o.TrailPct > 0 {
		return price <= o.TrailAnchor*(1-o.TrailPct)
	}
	return price <= o.TrailAnchor-o.TrailDistance
}

// fireTriggerLocked 执行一次触发：
// 触发瞬间重新校验资金/持仓；不足则以触发类拒单原因整单拒掉、释放全部冻结，不留多余占用；
// 足够则委托离开待触发集合，按触发后的委托类型立刻走普通撮合路径。
func (e *Engine) fireTriggerLocked(o *Order, price float64, eventID string, seq int64) {
	o.Triggered = true
	o.TriggerSeq = seq
	e.removeWaitingLocked(o)

	// 触发瞬间才冻结资金/持仓（触发前不占用，账户快照里只看到待触发状态而无对应冻结）。
	// 不够就整单拒掉，使用与普通下单不同的触发类拒单原因；拒绝路径不产生任何冻结。
	var reservePrice float64
	if o.Type == event.StopLimit {
		reservePrice = o.LimitPrice
	} else {
		reservePrice = price
	}
	o.ReservePrice = reservePrice
	if o.Side == event.Buy {
		if err := e.pf.ReserveBuy(o.Symbol, o.RemainingQty(), reservePrice); err != nil {
			e.rejectTriggerLocked(o, RejectTriggerNoCash, err.Error())
			return
		}
	} else {
		if err := e.pf.ReserveSell(o.Symbol, o.RemainingQty()); err != nil {
			e.rejectTriggerLocked(o, RejectTriggerNoPosition, err.Error())
			return
		}
	}

	o.Status = StatusTriggered
	switch o.Type {
	case event.StopLimit:
		// 转为普通限价单：进入活动簿，等待后续成交（DAY/GTC 可挂单，IOC/FOK 立即处理）。
		o.Status = StatusNew
		e.addActiveLocked(o)
		e.attemptImmediateLocked(o, eventID, seq)
	case event.StopMarket, event.TrailingStop:
		// 转为普通市价单：立刻在触发事件上成交，吃不完的剩余按市价单规则立即撤销。
		o.Status = StatusNew
		e.addActiveLocked(o)
		fills := e.tryMatchTradeLocked(o, price, 0, eventID, seq)
		if o.Status == StatusNew || o.Status == StatusPartiallyFilled {
			m := e.market[o.Symbol]
			if m != nil {
				fills = append(fills, e.tryMatchQuoteLocked(o, m, quoteDepth(o, m), eventID, seq)...)
			}
		}
		e.appendFillsLocked(eventID, fills)
		if o.Status == StatusNew || o.Status == StatusPartiallyFilled {
			if o.TIF == event.FOK {
				o.Status = StatusCanceled
				o.RejectReason = RejectFOKNotFilled
				e.removeActiveLocked(o)
				e.releaseReserveLocked(o, o.RemainingQty())
			} else if o.TIF == event.IOC {
				e.cancelRemainingLocked(o, StatusCanceled)
			} else {
				// 市价单不长期挂簿：剩余立即撤销，原因与既有市价单无流动性口径一致。
				o.RejectReason = RejectNoLiquidity
				e.cancelRemainingLocked(o, StatusCanceled)
			}
		}
	}
}

func (e *Engine) rejectTriggerLocked(o *Order, reason RejectReason, detail string) {
	o.Status = StatusRejected
	o.RejectReason = reason
	_ = detail
}

// attemptImmediateLocked 触发后的限价单按其 TIF 立即参与一次当前市场撮合。
func (e *Engine) attemptImmediateLocked(o *Order, eventID string, seq int64) {
	m := e.market[o.Symbol]
	var fills []Fill
	if m != nil {
		if m.hasTrade {
			fills = append(fills, e.tryMatchTradeLocked(o, m.tradePrice, m.tradeSize, eventID, seq)...)
		}
		if o.Status == StatusNew || o.Status == StatusPartiallyFilled {
			fills = append(fills, e.tryMatchQuoteLocked(o, m, quoteDepth(o, m), eventID, seq)...)
		}
	}
	e.appendFillsLocked(eventID, fills)
	if o.Status == StatusNew || o.Status == StatusPartiallyFilled {
		switch o.TIF {
		case event.IOC:
			e.cancelRemainingLocked(o, StatusCanceled)
		case event.FOK:
			o.Status = StatusExpired
			o.RejectReason = RejectFOKNotFilled
			e.removeActiveLocked(o)
			e.releaseReserveLocked(o, o.RemainingQty())
		}
	}
}

func (e *Engine) appendFillsLocked(eventID string, fills []Fill) {
	if len(fills) == 0 || eventID == "" {
		return
	}
	if r, ok := e.results[eventID]; ok {
		r.Fills = append(r.Fills, fills...)
	}
}

// sortedActiveLocked 返回某标的活动委托，按（Seq, ID）确定性排序。
func (e *Engine) sortedActiveLocked(symbol string) []*Order {
	bucket := e.active[symbol]
	orders := make([]*Order, 0, len(bucket))
	for _, o := range bucket {
		orders = append(orders, o)
	}
	sort.Slice(orders, func(i, j int) bool {
		if orders[i].Seq != orders[j].Seq {
			return orders[i].Seq < orders[j].Seq
		}
		return orders[i].ID < orders[j].ID
	})
	return orders
}

// matchOnTradeLocked 用一笔行情成交推动活动簿撮合。
// tradeRemaining 为该笔成交剩余可吃量（同一事件多委托共享）；传 0 表示不限制（触发即时撮合）。
func (e *Engine) matchOnTradeLocked(symbol, eventID string, seq int64) {
	m := e.market[symbol]
	remaining := m.tradeSize
	var fills []Fill
	for _, o := range e.sortedActiveLocked(symbol) {
		if remaining == 0 {
			break
		}
		fs := e.tryMatchTradeLocked(o, m.tradePrice, remaining, eventID, seq)
		fills = append(fills, fs...)
		for _, f := range fs {
			remaining -= f.Qty
		}
		if (o.Status == StatusNew || o.Status == StatusPartiallyFilled) && (o.TIF == event.IOC) {
			e.cancelRemainingLocked(o, StatusCanceled)
		}
	}
	e.appendFillsLocked(eventID, fills)
}

func (e *Engine) matchOnQuoteLocked(symbol, eventID string, seq int64) {
	m := e.market[symbol]
	var fills []Fill
	// 报价顶档可吃量按买卖侧共享：多张委托排队消费同一深度。
	depth := map[event.Side]int64{event.Buy: m.askSize, event.Sell: m.bidSize}
	for _, o := range e.sortedActiveLocked(symbol) {
		fs := e.tryMatchQuoteLocked(o, m, depth[o.Side], eventID, seq)
		fills = append(fills, fs...)
		for _, f := range fs {
			depth[o.Side] -= f.Qty
		}
		if (o.Status == StatusNew || o.Status == StatusPartiallyFilled) && o.TIF == event.IOC {
			e.cancelRemainingLocked(o, StatusCanceled)
		}
	}
	e.appendFillsLocked(eventID, fills)
}

func limitCrosses(o *Order, price float64) bool {
	if o.Type != event.Limit && o.Type != event.StopLimit {
		return true
	}
	if o.Side == event.Buy {
		return price <= o.LimitPrice
	}
	return price >= o.LimitPrice
}

// quoteDepth 返回单张委托立即撮合时该侧的盘口顶档可吃量。
func quoteDepth(o *Order, m *marketState) int64 {
	if o.Side == event.Buy {
		return m.askSize
	}
	return m.bidSize
}

// tryMatchTradeLocked 在给定成交事件上尝试撮合单张委托。
// FOK：可用量不足以全成时一张都不成（原子性）。
func (e *Engine) tryMatchTradeLocked(o *Order, price float64, available int64, eventID string, seq int64) []Fill {
	if o.Status != StatusNew && o.Status != StatusPartiallyFilled {
		return nil
	}
	if !limitCrosses(o, price) {
		return nil
	}
	want := o.RemainingQty()
	qty := want
	if available > 0 && available < qty {
		qty = available
	}
	if o.TIF == event.FOK && qty < want {
		return nil // 原子全成或全不成
	}
	if qty <= 0 {
		return nil
	}
	return e.applyMatchedFillLocked(o, qty, price, eventID, seq)
}

// tryMatchQuoteLocked 在报价簿顶档尝试撮合，available 为该侧剩余可吃量；
// 盘口未给量（depth==0）时按不限制处理，与既有语义一致。
func (e *Engine) tryMatchQuoteLocked(o *Order, m *marketState, available int64, eventID string, seq int64) []Fill {
	if (o.Status != StatusNew && o.Status != StatusPartiallyFilled) || !m.hasQuote {
		return nil
	}
	var price float64
	var depth int64
	if o.Side == event.Buy {
		price, depth = m.askPrice, m.askSize
	} else {
		price, depth = m.bidPrice, m.bidSize
	}
	if !limitCrosses(o, price) {
		return nil
	}
	qty := o.RemainingQty()
	if depth > 0 && available < qty {
		qty = available
	}
	if o.TIF == event.FOK && qty < o.RemainingQty() {
		return nil
	}
	if qty <= 0 {
		return nil
	}
	return e.applyMatchedFillLocked(o, qty, price, eventID, seq)
}

// applyMatchedFillLocked 落账一次成交：更新账户、推进委托状态机、离开活动簿（若已成满）。
func (e *Engine) applyMatchedFillLocked(o *Order, qty int64, price float64, eventID string, seq int64) []Fill {
	var err error
	if o.Side == event.Buy {
		err = e.pf.FillBuy(o.Symbol, qty, price, o.ReservePrice)
	} else {
		err = e.pf.FillSell(o.Symbol, qty, price)
	}
	if err != nil {
		// 理论上冻结阶段已保证不会发生；若发生，不静默吞掉：委托转拒单并释放剩余冻结。
		e.cancelRemainingLocked(o, StatusRejected)
		o.RejectReason = RejectInvalidEvent
		return nil
	}
	o.applyFill(qty, price)
	if o.Status.IsTerminal() {
		e.removeActiveLocked(o)
	}
	return []Fill{{
		EventID: eventID,
		Seq:     seq,
		OrderID: o.ID,
		Symbol:  o.Symbol,
		Side:    o.Side,
		Qty:     qty,
		Price:   price,
	}}
}
