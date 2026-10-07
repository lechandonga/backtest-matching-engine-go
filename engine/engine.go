package engine

// engine.go：事件驱动回测撮合引擎主循环。
//
// 确定性约定：
//   - 投递：Submit 只入缓冲；Drain/Process 把缓冲事件按 (Seq, EventID)
//     排序后顺序应用。因此同一批事件无论并发投递、乱序投递、重复投递
//     （相同 EventID 只生效一次）多少遍，最终状态完全一致。
//   - 不同标的：订单簿、触发条件按标的独立维护，互不影响。
//   - 收盘：一个收盘事件收口一整天（全部标的的当日单统一失效），
//     与事件是否携带标的无关；同一交易日重复收盘幂等。
//   - 触发：触发型委托在每个行情事件上至多评估一次；触发后进入普通
//     委托流程，且触发基准（含跟踪止损锚点）从此冻结。

import (
	"fmt"
	"sort"
	"sync"

	"github.com/lechandonga/backtest-matching-engine-go/event"
	"github.com/lechandonga/backtest-matching-engine-go/portfolio"
)

// Fill 是一笔成交回报。
type Fill struct {
	EventID string
	OrderID string
	Symbol  string
	Side    event.Side
	Price   float64
	Qty     int64
	Fee     float64
	Seq     int64
}

// OrderView 是订单快照视图（值拷贝）。
type OrderView struct {
	Order
}

// Result 描述单个事件的处理结果。重复事件返回首个结果的拷贝。
type Result struct {
	EventID string
	OrderID string
	Fills   []Fill
	// Reject 非 nil 表示该事件被拒（委托拒单或收盘事件非法）。
	Reject *RejectError
	// ExpiredOrderIDs 收盘事件导致失效的订单（含未成交完的当日单、未触发条件单）。
	ExpiredOrderIDs []string
}

// Engine 线程安全的撮合引擎。
type Engine struct {
	mu sync.Mutex

	pf *portfolio.Portfolio

	orders    map[string]*Order
	byEventID map[string]string // submit 事件 EventID -> OrderID

	// 每标的订单簿：按价格优先、同价按提交 Seq（时间优先）排列。
	bids map[string][]*Order
	asks map[string][]*Order

	// 等待触发的条件单（按标的）。
	waiters map[string]map[string]*Order

	// 每标的最新行情。
	bidPrice map[string]float64
	askPrice map[string]float64
	lastPx   map[string]float64

	// 已处理事件的结果（幂等）。
	results map[string]*Result

	pending []event.Event

	currentDay   int64 // 已收盘到的交易日序号
	lastCloseDay int64 // 最近一次已收口的交易日序号（整日收口，不按标的）

	dayCounter int64 // TradingDay=0 的收盘事件按此推断

	// 每个已收盘交易日固化的日终快照（重复收盘不改变内容）。
	daySnaps map[int64]dayEndRecord
}

// New 创建引擎。
func New(pf *portfolio.Portfolio) *Engine {
	return &Engine{
		pf:        pf,
		orders:    make(map[string]*Order),
		byEventID: make(map[string]string),
		bids:      make(map[string][]*Order),
		asks:      make(map[string][]*Order),
		waiters:   make(map[string]map[string]*Order),
		bidPrice:  make(map[string]float64),
		askPrice:  make(map[string]float64),
		lastPx:    make(map[string]float64),
		results:   make(map[string]*Result),
		daySnaps:  make(map[int64]dayEndRecord),
	}
}

// Submit 投递一批事件（线程安全）。事件只入缓冲，不改变任何状态。
func (e *Engine) Submit(evs ...event.Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pending = append(e.pending, evs...)
}

// Process 投递并立即按确定性顺序处理全部已缓冲事件，返回每个事件的结果。
func (e *Engine) Process(evs ...event.Event) []Result {
	e.Submit(evs...)
	return e.Drain()
}

// Drain 按 (Seq, EventID) 排序处理全部缓冲事件。
func (e *Engine) Drain() []Result {
	e.mu.Lock()
	defer e.mu.Unlock()

	batch := e.pending
	e.pending = nil
	sort.SliceStable(batch, func(i, j int) bool {
		if batch[i].Seq != batch[j].Seq {
			return batch[i].Seq < batch[j].Seq
		}
		return batch[i].EventID < batch[j].EventID
	})

	out := make([]Result, 0, len(batch))
	for _, ev := range batch {
		out = append(out, e.applyLocked(ev))
	}
	return out
}

// PendingCount 返回尚未处理的缓冲事件数。
func (e *Engine) PendingCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.pending)
}

func (e *Engine) applyLocked(ev event.Event) Result {
	if r, ok := e.results[ev.EventID]; ok {
		return copyResult(r)
	}

	var res Result
	res.EventID = ev.EventID

	if ev.EventID == "" || ev.Seq < 0 {
		res.Reject = reject(ev.OrderID, RejectInvalidEvent, "missing EventID or negative Seq")
		e.results[ev.EventID] = &res
		return res
	}

	switch ev.Kind {
	case event.KindQuote:
		e.applyQuoteLocked(ev, &res)
	case event.KindTrade:
		e.applyTradeLocked(ev, &res)
	case event.KindOrderSubmit:
		e.applySubmitLocked(ev, &res)
	case event.KindOrderCancel:
		e.applyCancelLocked(ev, &res)
	case event.KindSplit:
		if err := e.pf.ApplySplit(ev.Symbol, ev.SplitRatio, ev.PostSplit); err != nil {
			res.Reject = reject(ev.OrderID, RejectInvalidEvent, err.Error())
		}
	case event.KindDividend:
		if err := e.pf.ApplyDividend(ev.Symbol, ev.DivPerShare); err != nil {
			res.Reject = reject(ev.OrderID, RejectInvalidEvent, err.Error())
		}
	case event.KindDayClose:
		e.applyDayCloseLocked(ev, &res)
	default:
		res.Reject = reject(ev.OrderID, RejectInvalidEvent, fmt.Sprintf("unknown event kind %d", ev.Kind))
	}

	stored := res
	e.results[ev.EventID] = &stored
	return res
}

func copyResult(r *Result) Result {
	c := Result{EventID: r.EventID, OrderID: r.OrderID, Reject: r.Reject}
	c.ExpiredOrderIDs = append(c.ExpiredOrderIDs, r.ExpiredOrderIDs...)
	c.Fills = append(c.Fills, r.Fills...)
	return c
}

// CurrentTradingDay 返回已收盘到的交易日序号。
func (e *Engine) CurrentTradingDay() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.currentDay
}
