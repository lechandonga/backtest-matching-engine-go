// Package engine 实现事件驱动的回测撮合引擎。
//
// 并发与幂等（延续上一轮约定）：
//   - 所有公开入口都在单把互斥锁内串行化，任意并发投递最终状态一致；
//   - EventID 全局唯一，重复投递返回首次处理的结果（Duplicate=true），绝不二次生效；
//   - 每批事件先按 (Seq, EventID) 做确定性排序，再顺序处理，结果与投递顺序无关。
//
// 委托模型在普通市价/限价单之外，新增触发型委托（止损、止损限价、跟踪止损）、
// 有效期（DAY/IOC/FOK/GTC）与交易日收盘（KindSessionClose）。
package engine

import (
	"fmt"
	"sort"
	"sync"

	"github.com/lechandonga/backtest-matching-engine-go/event"
	"github.com/lechandonga/backtest-matching-engine-go/portfolio"
)

// Fill 描述一次成交回报。
type Fill struct {
	EventID string
	Seq     int64
	OrderID string
	Symbol  string
	Side    event.Side
	Qty     int64
	Price   float64
}

// Result 是单个事件的处理结果。重复事件返回首次结果并置 Duplicate。
type Result struct {
	EventID   string
	Accepted  bool
	Duplicate bool
	Reject    *RejectError
	Fills     []Fill
}

// OrderView 是委托在快照中的只读视图。
type OrderView struct {
	ID            string
	Symbol        string
	Side          event.Side
	Type          event.OrderType
	TIF           event.TimeInForce
	Qty           int64
	FilledQty     int64
	RemainingQty  int64
	AvgFillPrice  float64
	LimitPrice    float64
	StopPrice     float64
	TrailDistance float64
	TrailPct      float64
	TrailAnchor   float64
	Status        OrderStatus
	RejectReason  RejectReason
	Triggered     bool
	TradeDate     string
	Seq           int64
}

// DaySnapshot 日终快照：账户快照 + 收盘时的委托视图。
type DaySnapshot struct {
	TradeDate string
	Portfolio portfolio.Snapshot
	Orders    []OrderView
}

type marketState struct {
	bidPrice, askPrice float64
	bidSize, askSize   int64
	tradePrice         float64
	tradeSize          int64
	hasQuote, hasTrade bool
}

// Engine 事件驱动撮合引擎，线程安全。
type Engine struct {
	mu sync.Mutex

	pf *portfolio.Portfolio

	orders  map[string]*Order            // 全部委托（含终态，便于重复操作判定与快照）
	active  map[string]map[string]*Order // symbol -> orderID -> 活动委托（NEW/PARTIALLY_FILLED）
	waiting map[string]map[string]*Order // symbol -> orderID -> 待触发委托
	market  map[string]*marketState
	results map[string]*Result // EventID -> 首次处理结果

	tradeDate    string
	closedDates  map[string]bool
	daySnapshots map[string]*DaySnapshot
}

// New 创建引擎，pf 为已配置初始资金/费率的账户。
func New(pf *portfolio.Portfolio) *Engine {
	return &Engine{
		pf:           pf,
		orders:       make(map[string]*Order),
		active:       make(map[string]map[string]*Order),
		waiting:      make(map[string]map[string]*Order),
		market:       make(map[string]*marketState),
		results:      make(map[string]*Result),
		closedDates:  make(map[string]bool),
		daySnapshots: make(map[string]*DaySnapshot),
	}
}

// Process 处理一批事件：先确定性排序，再幂等、串行处理。
// 返回结果按入参顺序对齐（含重复投递标记），方便调用方逐条核对。
func (e *Engine) Process(events []event.Event) []Result {
	e.mu.Lock()
	defer e.mu.Unlock()

	ordered := make([]event.Event, len(events))
	copy(ordered, events)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Seq != ordered[j].Seq {
			return ordered[i].Seq < ordered[j].Seq
		}
		return ordered[i].EventID < ordered[j].EventID
	})
	for _, ev := range ordered {
		e.processOneLocked(ev)
	}

	out := make([]Result, len(events))
	for i, ev := range events {
		if r, ok := e.results[ev.EventID]; ok {
			out[i] = *r
		}
	}
	return out
}

// ProcessOne 处理单个事件。
func (e *Engine) ProcessOne(ev event.Event) Result {
	e.mu.Lock()
	defer e.mu.Unlock()
	r := e.processOneLocked(ev)
	return *r
}

func (e *Engine) processOneLocked(ev event.Event) *Result {
	if r, ok := e.results[ev.EventID]; ok {
		r.Duplicate = true
		return r
	}
	r := &Result{EventID: ev.EventID, Accepted: true}
	e.results[ev.EventID] = r

	if ev.EventID == "" || ev.Seq < 0 {
		e.rejectResult(r, "", RejectInvalidEvent, "empty event id or negative seq")
		return r
	}

	// 交易日推进：进入新交易日之前，若上一交易日未显式收盘，先自动收掉，
	// 保证隔夜仓位与当日单失效语义不因漏发收盘事件而破掉。
	if ev.TradeDate != "" && ev.TradeDate != e.tradeDate {
		if e.tradeDate != "" && !e.closedDates[e.tradeDate] {
			e.runSessionCloseLocked(e.tradeDate, "", 0, ev.EventID, ev.Seq)
		}
		e.tradeDate = ev.TradeDate
	}

	var err *RejectError
	switch ev.Kind {
	case event.KindQuote:
		err = e.handleQuoteLocked(ev)
	case event.KindTrade:
		err = e.handleTradeLocked(ev)
	case event.KindOrderSubmit:
		err = e.handleSubmitLocked(ev)
	case event.KindOrderCancel:
		err = e.handleCancelLocked(ev)
	case event.KindSplit:
		err = e.handleSplitLocked(ev)
	case event.KindDividend:
		err = e.handleDividendLocked(ev)
	case event.KindSessionClose:
		date := ev.TradeDate
		if date == "" {
			date = e.tradeDate
		}
		e.runSessionCloseLocked(date, ev.Symbol, ev.ClosePrice, ev.EventID, ev.Seq)
	default:
		err = reject("", RejectInvalidEvent, fmt.Sprintf("unknown event kind %d", ev.Kind))
	}
	if err != nil {
		e.rejectResult(r, err.OrderID, err.Reason, err.Detail)
	}
	return r
}

func (e *Engine) rejectResult(r *Result, orderID string, reason RejectReason, detail string) {
	r.Accepted = false
	r.Reject = reject(orderID, reason, detail)
	if orderID != "" {
		if o, ok := e.orders[orderID]; ok && !o.Status.IsTerminal() && o.Status != StatusWaitingTrigger {
			o.Status = StatusRejected
			o.RejectReason = reason
		}
	}
}

func (e *Engine) mk(symbol string) *marketState {
	m := e.market[symbol]
	if m == nil {
		m = &marketState{}
		e.market[symbol] = m
	}
	return m
}

func (e *Engine) rejectIfClosedLocked(symbol string) *RejectError {
	if e.tradeDate != "" && e.closedDates[e.tradeDate] {
		return reject(symbol, RejectSessionClosed, "trading session already closed: "+e.tradeDate)
	}
	return nil
}

func (e *Engine) handleQuoteLocked(ev event.Event) *RejectError {
	if err := e.rejectIfClosedLocked(ev.Symbol); err != nil {
		return err
	}
	if ev.BidPrice <= 0 || ev.AskPrice <= 0 || ev.BidPrice > ev.AskPrice {
		return reject(ev.Symbol, RejectInvalidPrice, "quote requires 0 < bid <= ask")
	}
	m := e.mk(ev.Symbol)
	m.bidPrice, m.askPrice = ev.BidPrice, ev.AskPrice
	m.bidSize, m.askSize = ev.BidSize, ev.AskSize
	m.hasQuote = true
	e.evaluateTriggersLocked(ev.Symbol, ev.EventID, ev.Seq)
	e.matchOnQuoteLocked(ev.Symbol, ev.EventID, ev.Seq)
	return nil
}

func (e *Engine) handleTradeLocked(ev event.Event) *RejectError {
	if err := e.rejectIfClosedLocked(ev.Symbol); err != nil {
		return err
	}
	if ev.TradePrice <= 0 || ev.TradeSize <= 0 {
		return reject(ev.Symbol, RejectInvalidPrice, "trade requires positive price and size")
	}
	m := e.mk(ev.Symbol)
	m.tradePrice, m.tradeSize = ev.TradePrice, ev.TradeSize
	m.hasTrade = true
	e.pf.SetMark(ev.Symbol, ev.TradePrice)
	e.evaluateTriggersLocked(ev.Symbol, ev.EventID, ev.Seq)
	e.matchOnTradeLocked(ev.Symbol, ev.EventID, ev.Seq)
	return nil
}

func (e *Engine) handleSplitLocked(ev event.Event) *RejectError {
	if ev.SplitRatio <= 0 {
		return reject(ev.Symbol, RejectInvalidEvent, "split ratio must be positive")
	}
	if err := e.pf.ApplySplit(ev.Symbol, ev.SplitRatio, ev.PostSplit); err != nil {
		return reject(ev.Symbol, RejectInvalidEvent, err.Error())
	}
	return nil
}

func (e *Engine) handleDividendLocked(ev event.Event) *RejectError {
	if ev.DivPerShare < 0 {
		return reject(ev.Symbol, RejectInvalidEvent, "dividend per share must be non-negative")
	}
	if err := e.pf.ApplyDividend(ev.Symbol, ev.DivPerShare); err != nil {
		return reject(ev.Symbol, RejectInvalidEvent, err.Error())
	}
	return nil
}

// Snapshot 返回当前账户快照与全部委托视图；账户拆不平时返回 ReconcileError。
func (e *Engine) Snapshot() (portfolio.Snapshot, []OrderView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	snap, err := e.pf.Snapshot()
	return snap, e.orderViewsLocked(), err
}

// GetDaySnapshot 返回指定交易日的收盘快照（未收盘返回 nil）。
func (e *Engine) GetDaySnapshot(date string) *DaySnapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	if s, ok := e.daySnapshots[date]; ok {
		cp := *s
		cp.Orders = append([]OrderView(nil), s.Orders...)
		return &cp
	}
	return nil
}

// ClosedDates 返回已完成收盘的交易日（有序）。
func (e *Engine) ClosedDates() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, 0, len(e.closedDates))
	for d := range e.closedDates {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

func (e *Engine) orderViewsLocked() []OrderView {
	out := make([]OrderView, 0, len(e.orders))
	for _, o := range e.orders {
		out = append(out, ViewOf(o))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Seq != out[j].Seq {
			return out[i].Seq < out[j].Seq
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// ViewOf 从运行时委托构造只读视图。
func ViewOf(o *Order) OrderView {
	return OrderView{
		ID:            o.ID,
		Symbol:        o.Symbol,
		Side:          o.Side,
		Type:          o.Type,
		TIF:           o.TIF,
		Qty:           o.Qty,
		FilledQty:     o.FilledQty,
		RemainingQty:  o.RemainingQty(),
		AvgFillPrice:  o.AvgFillPrice,
		LimitPrice:    o.LimitPrice,
		StopPrice:     o.StopPrice,
		TrailDistance: o.TrailDistance,
		TrailPct:      o.TrailPct,
		TrailAnchor:   o.TrailAnchor,
		Status:        o.Status,
		RejectReason:  o.RejectReason,
		Triggered:     o.Triggered,
		TradeDate:     o.TradeDate,
		Seq:           o.Seq,
	}
}

// TradingDate 返回引擎当前所处交易日。
func (e *Engine) TradingDate() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.tradeDate
}
