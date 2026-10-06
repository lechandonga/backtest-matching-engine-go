package engine

import (
	"github.com/lechandonga/backtest-matching-engine-go/event"
)

// runSessionCloseLocked 执行一个交易日的收盘：幂等、确定性。

// 同一交易日的收盘事件（含重复投递、乱序到达）只有第一次真正结算：
//   - 未成交完的 DAY 当日单、未触发的条件单统一失效（EXPIRED），释放冻结，不重复退钱退货；
//   - GTC 单（含已触发后的 GTC 活动单）跨日保留；
//   - 已处于终态的委托不再二次失效；
//   - 收盘事件携带的收盘价用于盯市；随后产出日终快照，账户拆不平直接返回对账错误。
func (e *Engine) runSessionCloseLocked(date, symbol string, closePrice float64, closeEventID string, seq int64) {
	if date == "" {
		return
	}
	if e.closedDates[date] {
		// 重复收盘：不再结算、不再失效、不再重复释放冻结。
		if r, ok := e.results[closeEventID]; ok {
			r.Duplicate = true
		}
		return
	}

	// 1) 收盘价盯市（先于失效与快照：快照权益按收盘价拆解）。
	if symbol != "" && closePrice > 0 {
		e.pf.SetMark(symbol, closePrice)
	}

	// 2) 失效该交易日尚未结束的当日单。待触发条件单若为 DAY 也一并失效。
	e.expireDayOrdersLocked(date)

	// 3) 标记收盘并记录日终快照（口径与逐笔 Snapshot 完全一致）。
	e.closedDates[date] = true

	snap, err := e.pf.Snapshot()
	if err != nil {
		// 拆不平必须明确报出：挂到触发收盘的事件结果上，绝不静默吸收。
		if r, ok := e.results[closeEventID]; ok {
			e.rejectResult(r, "", RejectInvalidEvent, err.Error())
		}
		return
	}
	e.daySnapshots[date] = &DaySnapshot{
		TradeDate: date,
		Portfolio: snap,
		Orders:    e.orderViewsLocked(),
	}
}

// expireDayOrdersLocked 让某交易日的 DAY 有效委托统一进入 EXPIRED 终态并释放剩余冻结。
// 已触发的条件单按其触发后类型处理：剩余的 GTC 保留，其余（DAY/IOC/FOK）失效。
func (e *Engine) expireDayOrdersLocked(date string) {
	// 待触发桶
	for symbol, bucket := range e.waiting {
		for id, o := range bucket {
			if !o.IsWaitingTrigger() {
				delete(bucket, id)
				continue
			}
			if o.TIF == event.GTC {
				continue
			}
			delete(bucket, id)
			e.expireOneLocked(o)
		}
		_ = symbol
	}
	// 活动桶
	for symbol, bucket := range e.active {
		for id, o := range bucket {
			if o.TIF == event.GTC {
				continue
			}
			delete(bucket, id)
			e.expireOneLocked(o)
		}
		_ = symbol
	}
	// 防御：清理空桶（保持结构整洁，不影响语义）。
	for symbol, bucket := range e.waiting {
		if len(bucket) == 0 {
			delete(e.waiting, symbol)
		}
	}
	for symbol, bucket := range e.active {
		if len(bucket) == 0 {
			delete(e.active, symbol)
		}
	}
}

// expireOneLocked 单张委托日终失效：释放全部剩余冻结，写入 EXPIRED 终态。
// 已带部分成交的委托保留成交，只退剩余部分，资金/持仓拆解仍然可解释。
func (e *Engine) expireOneLocked(o *Order) {
	rem := o.RemainingQty()
	// 待触发委托触发前不占用资金/持仓，无需释放；活动委托释放剩余冻结。
	if !o.IsWaitingTrigger() {
		e.releaseReserveLocked(o, rem)
	}
	o.Status = StatusExpired
	o.RejectReason = ""
}
