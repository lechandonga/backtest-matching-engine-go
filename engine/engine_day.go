package engine

import (
	"sort"

	"github.com/lechandonga/backtest-matching-engine-go/event"
)

// dayEndRecord 是某交易日收盘时固化的日终快照及对账结果。
type dayEndRecord struct {
	snap AccountSnapshot
	err  error // 权益拆解对不上时的 ReconcileError（不静默吞掉）
}

// applyDayCloseLocked 交易日收盘：一次收盘收口一整天，与事件是否携带
// 标的、携带哪个标的无关。
//
//   - 当天所有标的未成交完的当日单、未触发的当日条件单统一失效，
//     冻结资金/冻结持仓原路退回，不漏也不多退；
//   - 事件同时携带标的与正收盘价时，仅更新该标的的盯市价；
//     不带标的的收盘不会在行情/盯市里产生空标的记录；
//   - TradingDay 倒退拒绝（STALE_TRADING_DAY）；同一交易日重复收盘
//     （即使换了 EventID）幂等：不二次失效、不重复退钱退货、快照不变；
//   - 收盘完成后固化该交易日的日终快照（DayEndSnapshot 可取）。
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

	// 收盘价盯市：必须同时给出标的与正收盘价。
	if ev.Symbol != "" && ev.ClosePrice > 0 {
		e.lastPx[ev.Symbol] = ev.ClosePrice
		e.bidPrice[ev.Symbol] = ev.ClosePrice
		e.askPrice[ev.Symbol] = ev.ClosePrice
		e.pf.SetMark(ev.Symbol, ev.ClosePrice)
	}

	// 全部标的的当日单统一失效。先收集再按 (提交Seq, OrderID) 排序，
	// 保证失效顺序确定、与 map 遍历顺序无关。
	expiring := make([]*Order, 0)
	for _, w := range e.waiters {
		for _, o := range w {
			if o.TIF == event.TIODay {
				expiring = append(expiring, o)
			}
		}
	}
	for _, book := range e.bids {
		for _, o := range book {
			if o.TIF == event.TIODay {
				expiring = append(expiring, o)
			}
		}
	}
	for _, book := range e.asks {
		for _, o := range book {
			if o.TIF == event.TIODay {
				expiring = append(expiring, o)
			}
		}
	}
	sort.Slice(expiring, func(i, j int) bool {
		if expiring[i].Seq != expiring[j].Seq {
			return expiring[i].Seq < expiring[j].Seq
		}
		return expiring[i].ID < expiring[j].ID
	})

	for _, o := range expiring {
		switch o.Status {
		case StatusWaitingTrigger:
			// 触发前没有任何冻结，直接移出待触发集合。
			delete(e.waiters[o.Symbol], o.ID)
		case StatusNew, StatusPartiallyFilled:
			e.removeFromBookLocked(o, o.RemainingQty())
			e.releaseReserveLocked(o, o.RemainingQty())
		default:
			continue
		}
		o.Status = StatusExpired
		res.ExpiredOrderIDs = append(res.ExpiredOrderIDs, o.ID)
	}

	// 固化当日日终快照；对账失败随快照一起保存，取快照时明确报出。
	snap, err := e.snapshotLocked()
	e.daySnaps[day] = dayEndRecord{snap: snap, err: err}
}

// DayEndSnapshot 返回指定交易日收盘时固化的日终快照（含权益与现金、
// 冻结、持仓、已实现/未实现盈亏拆解）。ok=false 表示该交易日尚未收盘。
// err 为收盘对账时的 ReconcileError；权益拆解对不上时明确返回，不静默吞掉。
func (e *Engine) DayEndSnapshot(day int64) (snap AccountSnapshot, ok bool, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	rec, ok := e.daySnaps[day]
	if !ok {
		return AccountSnapshot{}, false, nil
	}
	return rec.snap, true, rec.err
}
