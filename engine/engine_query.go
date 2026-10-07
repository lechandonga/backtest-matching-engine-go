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
	return e.snapshotLocked()
}

// snapshotLocked 是 Snapshot 的无锁版本，供收盘固化日终快照等内部路径复用。
func (e *Engine) snapshotLocked() (AccountSnapshot, error) {
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
