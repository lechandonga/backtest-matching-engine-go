package engine

import (
	"sort"

	"github.com/lechandonga/backtest-matching-engine-go/event"
	"github.com/lechandonga/backtest-matching-engine-go/portfolio"
)

// dayEndRecord 是某个交易日收盘时结算并冻结的日终快照。
type dayEndRecord struct {
	snap portfolio.Snapshot
	err  error // 权益拆不平时的 ReconcileError（不静默吞掉）
}

// applyDayCloseLocked 交易日收盘（整日收口）：
//   - 一次收盘对全部标的生效，无论事件带不带标的、带的是哪个标的；
//   - 同一 TradingDay 重复收盘幂等，不二次失效、不重复退钱退货、不改写日终快照；
//   - DAY 单（未成交完的当日单、未触发条件单）统一失效并释放冻结；
//   - 事件带标的且收盘价 > 0 时更新该标的盯市价；不带标的不产生空标的记录；
//   - 收盘完成后结算并保存当日的日终快照（DayEndSnapshot 可取）。
func (e *Engine) applyDayCloseLocked(ev event.Event, res *Result) {
	day := ev.TradingDay
	if day == 0 {
		e.dayCounter++
		day = e.dayCounter
	}
	if day < e.lastCloseDay {
		res.Reject = reject(ev.OrderID, RejectStaleTradingDay,
			"trading day must be non-decreasing")
		return
	}
	if day == e.lastCloseDay {
		// 同一交易日重复收盘：幂等返回，不做任何状态变化。
		return
	}
	e.lastCloseDay = day
	if day > e.currentDay {
		e.currentDay = day
	}

	if ev.Symbol != "" && ev.ClosePrice > 0 {
		e.lastPx[ev.Symbol] = ev.ClosePrice
		e.bidPrice[ev.Symbol] = ev.ClosePrice
		e.askPrice[ev.Symbol] = ev.ClosePrice
		e.pf.SetMark(ev.Symbol, ev.ClosePrice)
	}

	e.expireDayOrdersLocked(res)

	// 收盘完成：结算并冻结当日日终快照。权益拆不平时 err 非 nil，
	// 由 DayEndSnapshot 透传给调用方，绝不静默吞掉。
	snap, err := e.pf.Snapshot()
	e.daySnapshots[day] = dayEndRecord{snap: snap, err: err}
}

// expireDayOrdersLocked 把全部标的的当日单统一失效：未触发的当日条件单、
// 未成交完的当日挂单，冻结资金/持仓原路退回。遍历顺序确定（标的字典序，
// 簿内按价格/时间优先），保证 ExpiredOrderIDs 可复现。
func (e *Engine) expireDayOrdersLocked(res *Result) {
	symbols := make([]string, 0, len(e.orders))
	seen := make(map[string]bool)
	for _, o := range e.orders {
		if !seen[o.Symbol] {
			seen[o.Symbol] = true
			symbols = append(symbols, o.Symbol)
		}
	}
	sort.Strings(symbols)

	for _, sym := range symbols {
		// 1) 未触发的当日条件单失效（触发前没有任何冻结）。
		w := e.waiters[sym]
		if len(w) > 0 {
			ids := make([]string, 0, len(w))
			for id := range w {
				ids = append(ids, id)
			}
			sort.Slice(ids, func(i, j int) bool {
				a, b := w[ids[i]], w[ids[j]]
				if a.Seq != b.Seq {
					return a.Seq < b.Seq
				}
				return a.ID < b.ID
			})
			for _, id := range ids {
				o := w[id]
				if o.TIF != event.TIODay {
					continue
				}
				delete(w, id)
				o.Status = StatusExpired
				res.ExpiredOrderIDs = append(res.ExpiredOrderIDs, o.ID)
			}
		}

		// 2) 未成交完的当日挂单失效，冻结干净退回。
		for _, side := range []event.Side{event.Buy, event.Sell} {
			book := e.bookFor(sym, side)
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
			e.setBook(sym, side, out)
		}
	}
}
