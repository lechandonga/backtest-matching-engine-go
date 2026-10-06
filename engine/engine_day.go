package engine

import (
	"github.com/lechandonga/backtest-matching-engine-go/event"
)

// applyDayCloseLocked 交易日收盘：
//   - 同一 (Symbol, TradingDay) 重复收盘幂等，不重复失效、不重复退钱退货；
//   - DAY 单（未成交完的当日单、未触发条件单）统一失效并释放冻结；
//   - 收盘价 > 0 时更新盯市价，隔夜仓位继续按新行情盯市；
//   - 不同标的的收盘互不影响。
func (e *Engine) applyDayCloseLocked(ev event.Event, res *Result) {
	day := ev.TradingDay
	if day == 0 {
		e.dayCounter++
		day = e.dayCounter
	}
	if day < e.lastClose[ev.Symbol] {
		res.Reject = reject(ev.OrderID, RejectStaleTradingDay,
			"trading day must be non-decreasing per symbol")
		return
	}
	if day == e.lastClose[ev.Symbol] {
		// 同一交易日重复收盘：幂等返回，不做任何状态变化。
		return
	}
	e.lastClose[ev.Symbol] = day
	if day > e.currentDay {
		e.currentDay = day
	}

	if ev.ClosePrice > 0 {
		e.lastPx[ev.Symbol] = ev.ClosePrice
		e.bidPrice[ev.Symbol] = ev.ClosePrice
		e.askPrice[ev.Symbol] = ev.ClosePrice
		e.pf.SetMark(ev.Symbol, ev.ClosePrice)
	}

	// 1) 未触发的当日条件单失效（触发前没有任何冻结）。
	for id, o := range e.waiters[ev.Symbol] {
		if o.TIF == event.TIODay {
			delete(e.waiters[ev.Symbol], id)
			o.Status = StatusExpired
			res.ExpiredOrderIDs = append(res.ExpiredOrderIDs, o.ID)
		}
	}

	// 2) 未成交完的当日挂单失效，冻结干净退回。
	for _, side := range []event.Side{event.Buy, event.Sell} {
		book := e.bookFor(ev.Symbol, side)
		out := book[:0]
		for _, o := range book {
			if o.TIF == event.TIODay {
				e.releaseReserveLocked(o, o.RemainingQty())
				o.Status = StatusExpired
				res.ExpiredOrderIDs = append(res.ExpiredOrderIDs, o.ID)
				continue
			}
			out = append(out, o)
		}
		e.setBook(ev.Symbol, side, out)
	}

	// 3) 已提交但尚未入簿/入待触发集合的 DAY 单（理论上不存在），兜底扫描。
	for _, o := range e.orders {
		if o.Symbol != ev.Symbol || o.TIF != event.TIODay {
			continue
		}
		switch o.Status {
		case StatusNew, StatusPartiallyFilled, StatusWaitingTrigger:
			if o.Status == StatusWaitingTrigger {
				delete(e.waiters[o.Symbol], o.ID)
			}
			e.removeFromBookLocked(o, o.RemainingQty())
			e.releaseReserveLocked(o, o.RemainingQty())
			o.Status = StatusExpired
			res.ExpiredOrderIDs = append(res.ExpiredOrderIDs, o.ID)
		}
	}
}
