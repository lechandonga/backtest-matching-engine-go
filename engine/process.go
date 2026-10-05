package engine

import (
	"sort"

	"github.com/lechandonga/backtest-matching-engine-go/account"
	"github.com/lechandonga/backtest-matching-engine-go/event"
)

// Process 处理一条事件。业务拒绝（非法状态、资金不足等）体现在 Result.Reject；
// 同一 EventID 重复投递返回 Duplicate=true 且不产生任何副作用。
func (e *Engine) Process(ev *event.Event) (*Result, error) {
	if err := ev.Validate(); err != nil {
		reason := RejectInvalidEvent
		if ev.Kind == event.KindSubmit && event.IsInvalidPriceErr(err) {
			reason = RejectInvalidPrice
		}
		return &Result{
			Seq:     seqOf(ev),
			EventID: ev.EventID,
			Reject:  reject(reason, "%s", err.Error()),
		}, nil
	}

	e.mu.Lock()
	if prior, ok := e.seen[ev.EventID]; ok {
		res := e.dupResult(ev, prior)
		e.mu.Unlock()
		return res, nil
	}
	e.seen[ev.EventID] = ev.Seq
	e.mu.Unlock()

	var (
		res *Result
		err error
	)
	switch ev.Kind {
	case event.KindMarket:
		res, err = e.processMarket(ev)
	case event.KindSubmit:
		res, err = e.processSubmit(ev)
	case event.KindCancel:
		res, err = e.processCancel(ev)
	case event.KindSplit:
		res, err = e.processSplit(ev)
	case event.KindDividend:
		res, err = e.processDividend(ev)
	}
	if err != nil {
		return nil, err
	}
	if res == nil {
		res = &Result{Seq: ev.Seq, EventID: ev.EventID}
	}
	res.Seq = ev.Seq
	res.EventID = ev.EventID
	return res, nil
}

func (e *Engine) dupResult(ev *event.Event, priorSeq int64) *Result {
	res := &Result{Seq: ev.Seq, EventID: ev.EventID, Duplicate: true}
	if oid, ok := e.orderByEvt[ev.EventID]; ok {
		res.OrderID = oid
		if o, ok2 := e.orders[oid]; ok2 {
			res.Status = o.Status
		}
	}
	_ = priorSeq
	return res
}

func seqOf(ev *event.Event) int64 {
	if ev == nil {
		return 0
	}
	return ev.Seq
}

func (e *Engine) processMarket(ev *event.Event) (*Result, error) {
	b := e.book(ev.Symbol)
	b.mu.Lock()
	defer b.mu.Unlock()

	if ev.LastPrice > 0 {
		b.last = ev.LastPrice
		b.hasPrice = true
	}
	liq := cloneLiquidity(ev.Liquidity)

	var fills []Fill
	// 外部报价驱动挂簿撮合：买单吃下穿的卖报价，卖单吃下穿的买报价。
	fills = append(fills, e.sweepBookLocked(b, ev.Symbol, ev.Seq, liq)...)

	if len(liq) > 0 {
		// 未被吃掉的外部流动性作为最新一档快照保留，供后续市价单用。
		b.last = ev.LastPrice
	}
	return &Result{Fills: fills}, nil
}

func (e *Engine) processSubmit(ev *event.Event) (*Result, error) {
	od := ev.Order
	b := e.book(od.Symbol)
	b.mu.Lock()
	defer b.mu.Unlock()

	e.mu.Lock()
	if _, exists := e.orders[od.ID]; exists {
		e.mu.Unlock()
		return &Result{Reject: reject(RejectDuplicateOrderID, "order %s already exists", od.ID)}, nil
	}
	e.mu.Unlock()

	o := &Order{
		ID:       od.ID,
		Symbol:   od.Symbol,
		Side:     string(od.Side),
		Type:     string(od.Type),
		Qty:      od.Qty,
		Price:    od.Price,
		Status:   StatusNew,
		EventSeq: ev.Seq,
	}

	// 资金/持仓校验：不满足直接拒单，订单进入 Rejected 终态。
	if od.Side == event.Buy {
		refPrice := od.Price
		if od.Type == event.Market {
			p, ok := worstBuyPriceLocked(b)
			if !ok {
				o.Status = StatusRejected
				o.Reject = reject(RejectNoMarketPrice, "market buy %s has no reference price", od.ID)
				e.storeRejected(ev, o)
				return &Result{OrderID: o.ID, Status: o.Status, Reject: o.Reject}, nil
			}
			refPrice = p
		}
		if err := e.acct.PlaceBuy(o.ID, o.Symbol, o.Qty, refPrice, e.fee); err != nil {
			o.Status = StatusRejected
			o.Reject = mapAcctErr(err, od)
			e.storeRejected(ev, o)
			return &Result{OrderID: o.ID, Status: o.Status, Reject: o.Reject}, nil
		}
	} else {
		if err := e.acct.PlaceSell(o.ID, o.Symbol, o.Qty); err != nil {
			o.Status = StatusRejected
			o.Reject = mapAcctErr(err, od)
			e.storeRejected(ev, o)
			return &Result{OrderID: o.ID, Status: o.Status, Reject: o.Reject}, nil
		}
	}

	e.mu.Lock()
	e.orders[o.ID] = o
	e.orderByEvt[ev.EventID] = o.ID
	e.mu.Unlock()

	var fills []Fill
	// 主动单先吃外部流动性，再吃对手挂簿。
	liq := currentExternalLocked(b)
	fills = append(fills, e.sweepAggressiveLocked(b, o, ev.Seq, liq)...)

	if o.Type == string(event.Market) && o.Remaining() > 0 {
		// 市价单为 IOC：有成交则部分撤，无成交则拒单（无流动性）。
		if o.FilledQty == 0 {
			_ = e.acct.Release(o.ID)
			if err := o.transition(StatusRejected); err != nil {
				return nil, err
			}
			o.Reject = reject(RejectNoLiquidity, "market order %s unmatched", o.ID)
			return &Result{OrderID: o.ID, Status: o.Status, Fills: fills, Reject: o.Reject}, nil
		}
		if err := e.releaseReservationLocked(o); err != nil {
			return nil, err
		}
		if err := o.transition(StatusPartiallyCanceled); err != nil {
			return nil, err
		}
	} else if o.Remaining() > 0 && o.Type == string(event.Limit) {
		rest := &bookOrder{order: o, seq: ev.Seq}
		if o.Side == string(event.Buy) {
			b.bids = append(b.bids, rest)
			sortSide(b.bids, true)
		} else {
			b.asks = append(b.asks, rest)
			sortSide(b.asks, false)
		}
	}

	return &Result{OrderID: o.ID, Status: o.Status, Fills: fills}, nil
}

func (e *Engine) processCancel(ev *event.Event) (*Result, error) {
	b := e.book(ev.CancelSymbol)
	b.mu.Lock()
	defer b.mu.Unlock()

	e.mu.Lock()
	o, ok := e.orders[ev.CancelOrderID]
	e.mu.Unlock()
	if !ok {
		return &Result{Reject: reject(RejectUnknownOrder, "order %s not found", ev.CancelOrderID)}, nil
	}
	if o.Terminal() {
		return &Result{OrderID: o.ID, Status: o.Status,
			Reject: reject(RejectOrderTerminal, "order %s is %s", o.ID, o.Status)}, nil
	}

	remaining := o.Remaining()
	if remaining <= 0 {
		return &Result{OrderID: o.ID, Status: o.Status,
			Reject: reject(RejectNothingToCancel, "order %s has no remaining qty", o.ID)}, nil
	}
	cancelQty := ev.CancelQty
	if cancelQty == 0 {
		cancelQty = remaining
	}
	if cancelQty > remaining+1e-12 {
		return &Result{OrderID: o.ID, Status: o.Status,
			Reject: reject(RejectCancelExceedsQty, "cancel %.6f > remaining %.6f", cancelQty, remaining)}, nil
	}

	// 从挂簿移除（全撤移除整单；部分撤缩减数量，保留价格时间优先级）。
	if cancelQty >= remaining-1e-12 {
		removeFromBook(b, o)
		if err := e.releaseReservationLocked(o); err != nil {
			return nil, err
		}
		target := StatusCanceled
		if o.FilledQty > 0 {
			target = StatusPartiallyCanceled
		}
		if err := o.transition(target); err != nil {
			return nil, err
		}
	} else {
		o.Qty -= cancelQty
		if o.Side == string(event.Buy) {
			if err := e.acct.AdjustBuyReservation(o.ID, o.Symbol, o.Remaining(), o.Price, e.fee); err != nil {
				return nil, err
			}
		} else {
			if err := e.acct.AdjustSellReservation(o.ID, o.Symbol, o.Remaining()); err != nil {
				return nil, err
			}
		}
	}
	return &Result{OrderID: o.ID, Status: o.Status}, nil
}

func (e *Engine) processSplit(ev *event.Event) (*Result, error) {
	sym, ratio := ev.CorporateSymbol, ev.Ratio
	b := e.book(sym)
	b.mu.Lock()
	defer b.mu.Unlock()

	e.mu.Lock()
	var open []*Order
	for _, o := range e.orders {
		if o.Symbol == sym && !o.Terminal() {
			open = append(open, o)
		}
	}
	sort.Slice(open, func(i, j int) bool { return open[i].EventSeq < open[j].EventSeq })
	e.mu.Unlock()

	// 买单预留金额依赖价格/数量，先释放，调完再重设；卖单预留由 account.Split 同步。
	for _, o := range open {
		if o.Side == string(event.Buy) {
			if err := e.acct.Release(o.ID); err != nil {
				return nil, err
			}
		}
	}
	e.acct.Split(ev.EventID, sym, ratio)
	if b.hasPrice {
		b.last /= ratio
	}
	for _, o := range open {
		o.Qty = o.FilledQty + (o.Qty-o.FilledQty)*ratio
		if o.Type == string(event.Limit) {
			o.Price /= ratio
		}
		if o.Side == string(event.Buy) {
			ref := o.Price
			if o.Type == string(event.Market) {
				ref = b.last
			}
			if err := e.acct.PlaceBuy(o.ID, sym, o.Remaining(), ref, e.fee); err != nil {
				return nil, err
			}
		}
	}
	sortSide(b.bids, true)
	sortSide(b.asks, false)
	return &Result{}, nil
}

func (e *Engine) processDividend(ev *event.Event) (*Result, error) {
	e.acct.Dividend(ev.EventID, ev.CorporateSymbol, ev.PerShareCash)
	return &Result{}, nil
}

func (e *Engine) storeRejected(ev *event.Event, o *Order) {
	e.mu.Lock()
	defer e.mu.Unlock()
	o.Status = StatusRejected
	e.orders[o.ID] = o
	e.orderByEvt[ev.EventID] = o.ID
}

func (e *Engine) releaseReservationLocked(o *Order) error {
	return e.acct.Release(o.ID)
}

func mapAcctErr(err error, od event.Order) *Reject {
	switch {
	case errorIs(err, account.ErrInsufficientCash):
		return reject(RejectInsufficientCash, "%s", err.Error())
	case errorIs(err, account.ErrInsufficientPosition):
		return reject(RejectInsufficientPos, "%s", err.Error())
	default:
		return reject(RejectInvalidEvent, "%s", err.Error())
	}
}

func removeFromBook(b *symbolBook, o *Order) {
	b.bids = removeOne(b.bids, o)
	b.asks = removeOne(b.asks, o)
}

func removeOne(side []*bookOrder, o *Order) []*bookOrder {
	for i, bo := range side {
		if bo.order == o {
			return append(side[:i], side[i+1:]...)
		}
	}
	return side
}

func cloneLiquidity(in []event.Liquidity) []event.Liquidity {
	if len(in) == 0 {
		return nil
	}
	out := make([]event.Liquidity, len(in))
	copy(out, in)
	return out
}
