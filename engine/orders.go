package engine

import (
	"github.com/lechandonga/backtest-matching-engine-go/event"
)

func validTIF(t event.TimeInForce) bool {
	return t == 0 || t == event.Day || t == event.IOC || t == event.FOK || t == event.GTC
}

// validateSubmitLocked 校验委托字段，并确定冻结参考价。
// 触发型委托在下订阶段同样冻结资金/持仓（待触发状态也要在账户快照中可解释），
// 触发瞬间再重新校验资金/持仓是否仍然充足，不足则以触发类拒单原因拒掉并释放冻结。
func (e *Engine) handleSubmitLocked(ev event.Event) *RejectError {
	if err := e.rejectIfClosedLocked(ev.Symbol); err != nil {
		return err
	}
	if ev.OrderID == "" {
		return reject("", RejectInvalidEvent, "submit requires order id")
	}
	if _, exists := e.orders[ev.OrderID]; exists {
		return reject(ev.OrderID, RejectDuplicateOrderID, "order id already exists")
	}
	if ev.Side != event.Buy && ev.Side != event.Sell {
		return reject(ev.OrderID, RejectInvalidSide, "side must be BUY or SELL")
	}
	if ev.Qty <= 0 {
		return reject(ev.OrderID, RejectInvalidQty, "qty must be positive")
	}
	if !validTIF(ev.TIF) {
		return reject(ev.OrderID, RejectInvalidTIF, "unsupported time in force")
	}

	tif := ev.TIF
	if tif == 0 {
		tif = event.Day // 零值兼容旧事件
	}

	o := &Order{
		ID:            ev.OrderID,
		Symbol:        ev.Symbol,
		Side:          ev.Side,
		Type:          ev.Type,
		LimitPrice:    ev.Price,
		Qty:           ev.Qty,
		Status:        StatusNew,
		Seq:           ev.Seq,
		TIF:           tif,
		StopPrice:     ev.StopPrice,
		TrailDistance: ev.TrailDistance,
		TrailPct:      ev.TrailPct,
		TradeDate:     ev.TradeDate,
	}

	var reservePrice float64
	switch ev.Type {
	case event.Market:
		m := e.market[ev.Symbol]
		if m == nil || (!m.hasQuote && !m.hasTrade) {
			return reject(ev.OrderID, RejectNoMarketData, "market order requires existing market data")
		}
		reservePrice = reserveReference(ev.Side, m)
		o.ReservePrice = reservePrice
	case event.Limit:
		if ev.Price <= 0 {
			return reject(ev.OrderID, RejectInvalidLimitPrice, "limit order requires positive price")
		}
		reservePrice = ev.Price
		o.ReservePrice = reservePrice
	case event.StopMarket:
		if ev.StopPrice <= 0 {
			return reject(ev.OrderID, RejectInvalidStopPrice, "stop order requires positive stop price")
		}
		reservePrice = ev.StopPrice
		o.ReservePrice = reservePrice
	case event.StopLimit:
		if ev.StopPrice <= 0 {
			return reject(ev.OrderID, RejectInvalidStopPrice, "stop order requires positive stop price")
		}
		if ev.Price <= 0 {
			return reject(ev.OrderID, RejectInvalidLimitPrice, "stop-limit requires positive limit price")
		}
		// 冻结按更保守口径：买入按 max(触发价, 限价)，卖出按 min(触发价, 限价)。
		if ev.Side == event.Buy {
			reservePrice = ev.StopPrice
			if ev.Price > reservePrice {
				reservePrice = ev.Price
			}
		} else {
			reservePrice = ev.StopPrice
			if ev.Price < reservePrice {
				reservePrice = ev.Price
			}
		}
		o.ReservePrice = reservePrice
	case event.TrailingStop:
		if ev.TrailDistance <= 0 && ev.TrailPct <= 0 {
			return reject(ev.OrderID, RejectInvalidTrail, "trailing stop requires positive trail distance or pct")
		}
		if (ev.TrailDistance < 0 || ev.TrailPct < 0) || (ev.TrailDistance > 0 && ev.TrailPct > 0) {
			return reject(ev.OrderID, RejectInvalidTrail, "trail distance and pct are mutually exclusive and non-negative")
		}
		// 跟踪止损下订时还没有有利基准；用当前最新可得对手价作为冻结参考，
		// 没有任何行情则拒绝——避免无界冻结。
		m := e.market[ev.Symbol]
		if m == nil || (!m.hasQuote && !m.hasTrade) {
			return reject(ev.OrderID, RejectNoMarketData, "trailing stop requires existing market data for reserve")
		}
		reservePrice = reserveReference(ev.Side, m)
		o.ReservePrice = reservePrice
		o.TrailAnchor = reservePrice
	default:
		return reject(ev.OrderID, RejectInvalidType, "unsupported order type")
	}

	// 触发型委托在触发前先进入待触发状态；其余直接为 NEW。
	triggerOrder := o.IsTriggerOrder()
	if triggerOrder {
		o.Status = StatusWaitingTrigger
	}

	// 下订冻结：触发型委托触发前不占用资金/持仓（快照里仅以 WAITING_TRIGGER 可见），
	// 冻结发生在触发瞬间，不足则以触发类原因拒单；普通委托下订即冻结。
	if !triggerOrder {
		if ev.Side == event.Buy {
			if err := e.pf.ReserveBuy(ev.Symbol, ev.Qty, reservePrice); err != nil {
				return reject(ev.OrderID, RejectInsufficientCash, err.Error())
			}
		} else {
			if err := e.pf.ReserveSell(ev.Symbol, ev.Qty); err != nil {
				return reject(ev.OrderID, RejectInsufficientPos, err.Error())
			}
		}
	}

	e.orders[o.ID] = o
	if triggerOrder {
		e.addWaitingLocked(o)
	} else {
		e.addActiveLocked(o)
	}

	// 非触发型委托：若已有行情，立即尝试成交一次（IOC/FOK 的“立即”语义发生在这里）。
	if !triggerOrder {
		m := e.market[ev.Symbol]
		var fills []Fill
		if m != nil {
			if m.hasTrade {
				fills = append(fills, e.tryMatchTradeLocked(o, m.tradePrice, m.tradeSize, ev.EventID, ev.Seq)...)
			}
			if o.Status == StatusNew || o.Status == StatusPartiallyFilled {
				fills = append(fills, e.tryMatchQuoteLocked(o, m, quoteDepth(o, m), ev.EventID, ev.Seq)...)
			}
		}
		e.appendFillsLocked(ev.EventID, fills)
		if o.Status == StatusNew || o.Status == StatusPartiallyFilled {
			switch tif {
			case event.IOC:
				e.cancelRemainingLocked(o, StatusCanceled)
			case event.FOK:
				// FOK 原子性由 tryMatch* 保证：不可能部分成交；走到这里说明整单未成。
				if o.Status == StatusNew {
					o.Status = StatusCanceled
					o.RejectReason = RejectFOKNotFilled
					e.removeActiveLocked(o)
					e.releaseReserveLocked(o, o.RemainingQty())
				}
			default:
				if o.Type == event.Market {
					// 市价单不长期挂簿：吃不完的剩余立即撤销（沿用 NO_LIQUIDITY 口径）。
					o.RejectReason = RejectNoLiquidity
					e.cancelRemainingLocked(o, StatusCanceled)
				}
			}
		}
	}
	return nil
}

func reserveReference(side event.Side, m *marketState) float64 {
	if side == event.Buy {
		if m.hasQuote {
			return m.askPrice
		}
		return m.tradePrice
	}
	if m.hasQuote {
		return m.bidPrice
	}
	return m.tradePrice
}

func (e *Engine) handleCancelLocked(ev event.Event) *RejectError {
	o := e.orders[ev.OrderID]
	if o == nil {
		return reject(ev.OrderID, RejectUnknownOrder, "cancel target does not exist")
	}
	if o.Status == StatusExpired {
		return reject(ev.OrderID, RejectExpiredOrder, "order already expired")
	}
	if o.Status.IsTerminal() {
		return reject(ev.OrderID, RejectTerminalOrder, "cannot operate on terminal order")
	}
	if !canCancelStatus(o.Status) {
		return reject(ev.OrderID, RejectTerminalOrder, "order not cancellable in status "+o.Status.String())
	}
	if ev.Qty < 0 {
		return reject(ev.OrderID, RejectInvalidQty, "cancel qty must be non-negative")
	}
	cancelQty := o.RemainingQty()
	if ev.Qty > 0 {
		if ev.Qty > cancelQty {
			return reject(ev.OrderID, RejectCancelExceedQty, "cancel qty exceeds remaining")
		}
		cancelQty = ev.Qty
	}

	// 待触发委托可整单撤销；部分撤单（Qty>0）只缩减数量，仍保留待触发。
	if o.IsWaitingTrigger() {
		if ev.Qty > 0 && ev.Qty < o.RemainingQty() {
			// 待触发委托尚未冻结资金/持仓，部分撤单只缩减委托数量。
			o.Qty -= cancelQty
			return nil
		}
		e.removeWaitingLocked(o)
		o.Status = StatusCanceled
		return nil
	}

	if ev.Qty > 0 && ev.Qty < o.RemainingQty() {
		// 部分撤单：缩减冻结规模，委托继续有效（旧语义）。
		if err := e.shrinkActiveLocked(o, cancelQty); err != nil {
			return reject(o.ID, RejectInvalidEvent, err.Error())
		}
		return nil
	}

	e.cancelRemainingLocked(o, StatusCanceled)
	return nil
}

func (e *Engine) addActiveLocked(o *Order) {
	s := e.active[o.Symbol]
	if s == nil {
		s = make(map[string]*Order)
		e.active[o.Symbol] = s
	}
	s[o.ID] = o
}

func (e *Engine) removeActiveLocked(o *Order) {
	if s := e.active[o.Symbol]; s != nil {
		delete(s, o.ID)
	}
}

func (e *Engine) addWaitingLocked(o *Order) {
	s := e.waiting[o.Symbol]
	if s == nil {
		s = make(map[string]*Order)
		e.waiting[o.Symbol] = s
	}
	s[o.ID] = o
}

func (e *Engine) removeWaitingLocked(o *Order) {
	if s := e.waiting[o.Symbol]; s != nil {
		delete(s, o.ID)
	}
}

// releaseReserveLocked 释放指定剩余数量对应的冻结（买入资金/卖出持仓）。
func (e *Engine) releaseReserveLocked(o *Order, qty int64) {
	if qty <= 0 {
		return
	}
	if o.Side == event.Buy {
		e.pf.ReleaseBuy(o.ReservePrice, qty)
	} else {
		e.pf.ReleaseSell(o.Symbol, qty)
	}
}

// cancelRemainingLocked 撤销活动委托全部剩余数量并释放冻结，允许带部分成交。
func (e *Engine) cancelRemainingLocked(o *Order, status OrderStatus) {
	rem := o.RemainingQty()
	e.removeActiveLocked(o)
	// 收口委托总量：撤销后剩余可成交数量必须为 0，快照口径才可解释。
	o.Qty = o.FilledQty
	o.Status = status
	e.releaseReserveLocked(o, rem)
}

// shrinkActiveLocked 活动委托部分撤单：缩减委托总量并释放对应冻结。
func (e *Engine) shrinkActiveLocked(o *Order, qty int64) error {
	// 用“先释放 qty 冻结 + 下调 Qty”的方式保持账户恒等式。
	if o.Side == event.Buy {
		e.pf.ReleaseBuy(o.ReservePrice, qty)
	} else {
		e.pf.ReleaseSell(o.Symbol, qty)
	}
	o.Qty -= qty
	return nil
}
