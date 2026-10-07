package engine

import (
	"sort"

	"github.com/lechandonga/backtest-matching-engine-go/portfolio"
)

// AccountSnapshot 引擎视角的账户+委托快照。待触发单单独成组，
// 便于在账户快照里看出"还挂在待触发状态"。
type AccountSnapshot struct {
	Portfolio      portfolio.Snapshot
	TradingDay     int64
	Orders         []OrderView
	WaitingTrigger []OrderView
	OpenOrders     []OrderView
}

// Snapshot 产出一份快照并执行权益对账；拆不平时 portfolio 返回 ReconcileError。
func (e *Engine) Snapshot() (AccountSnapshot, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	pfSnap, err := e.pf.Snapshot()
	if err != nil {
		return AccountSnapshot{Portfolio: pfSnap, TradingDay: e.currentDay}, err
	}
	out := AccountSnapshot{Portfolio: pfSnap, TradingDay: e.currentDay}

	ids := make([]string, 0, len(e.orders))
	for id := range e.orders {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		o := e.orders[id]
		v := OrderView{*o}
		out.Orders = append(out.Orders, v)
		switch o.Status {
		case StatusWaitingTrigger:
			out.WaitingTrigger = append(out.WaitingTrigger, v)
		case StatusNew, StatusPartiallyFilled:
			out.OpenOrders = append(out.OpenOrders, v)
		}
	}
	return out, nil
}

// Order 查询单个订单当前状态，ok=false 表示不存在。
func (e *Engine) Order(id string) (OrderView, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	o, ok := e.orders[id]
	if !ok {
		return OrderView{}, false
	}
	return OrderView{*o}, true
}

// DayEndSnapshot 返回某个已收盘交易日收盘时结算并冻结的日终快照。
// ok=false 表示该交易日尚未收盘。err 非 nil 表示当日权益拆解对不上
// （portfolio.ReconcileError），快照仍一并返回，绝不静默吞掉。
// 同一交易日重复收盘不会改写该快照。
func (e *Engine) DayEndSnapshot(day int64) (portfolio.Snapshot, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	rec, ok := e.daySnapshots[day]
	if !ok {
		return portfolio.Snapshot{}, false, nil
	}
	return rec.snap, true, rec.err
}
