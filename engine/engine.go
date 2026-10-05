package engine

import (
	"fmt"
	"sort"
	"sync"

	"github.com/lechandonga/backtest-matching-engine-go/account"
	"github.com/lechandonga/backtest-matching-engine-go/event"
)

// Fill 是引擎产出的成交（含全局序号，保证回放可解释）。
type Fill struct {
	ID      string
	Seq     int64
	Symbol  string
	OrderID string
	Side    string
	Qty     float64
	Price   float64
	Fee     float64
}

// Result 是处理一条事件的结果。重复投递的事件 Duplicate=true 且不产生副作用。
type Result struct {
	Seq       int64
	EventID   string
	Duplicate bool
	Fills     []Fill
	OrderID   string
	Status    OrderStatus
	Reject    *Reject
}

type bookOrder struct {
	order *Order
	seq   int64
}

type symbolBook struct {
	mu       sync.Mutex
	last     float64
	hasPrice bool
	external []event.Liquidity // 最近一次行情提供的可成交外部流动性
	bids     []*bookOrder      // 买限：价格降序、同价 seq 升序
	asks     []*bookOrder      // 卖限：价格升序、同价 seq 升序
}

// Engine 是事件驱动撮合引擎。
type Engine struct {
	mu sync.Mutex // 只保护 map 与 seen/fills 计数

	acct  *account.Account
	fee   account.FeeModel
	books map[string]*symbolBook

	orders     map[string]*Order
	orderByEvt map[string]string // eventID -> orderID
	seen       map[string]int64  // eventID -> seq
	fillSeq    int64
}

// Option 配置引擎。
type Option func(*Engine)

// WithFeeModel 设置费用模型，默认零费用。
func WithFeeModel(f account.FeeModel) Option {
	return func(e *Engine) { e.fee = f }
}

// New 创建引擎。
func New(acct *account.Account, opts ...Option) *Engine {
	e := &Engine{
		acct:       acct,
		fee:        account.FeeFunc(account.ZeroFee),
		books:      make(map[string]*symbolBook),
		orders:     make(map[string]*Order),
		orderByEvt: make(map[string]string),
		seen:       make(map[string]int64),
	}
	for _, o := range opts {
		o(e)
	}
	return e
}

// Account 返回底层账户。
func (e *Engine) Account() *account.Account { return e.acct }

// Order 查询订单当前视图（副本）。
func (e *Engine) Order(id string) (Order, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	o, ok := e.orders[id]
	if !ok {
		return Order{}, false
	}
	return *o, true
}

// LastPrice 返回标的最新价。
func (e *Engine) LastPrice(symbol string) (float64, bool) {
	b := e.book(symbol)
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.last, b.hasPrice
}

func (e *Engine) book(symbol string) *symbolBook {
	e.mu.Lock()
	defer e.mu.Unlock()
	b, ok := e.books[symbol]
	if !ok {
		b = &symbolBook{}
		e.books[symbol] = b
	}
	return b
}

func (e *Engine) newFillID(seq int64, n int) string {
	return fmt.Sprintf("F%06d-%d", seq, n)
}

// sortBook 重排一侧订单簿。
func sortSide(side []*bookOrder, buy bool) {
	sort.SliceStable(side, func(i, j int) bool {
		if side[i].order.Price != side[j].order.Price {
			if buy {
				return side[i].order.Price > side[j].order.Price
			}
			return side[i].order.Price < side[j].order.Price
		}
		return side[i].seq < side[j].seq
	})
}

func sprintf(format string, args ...any) string { return fmt.Sprintf(format, args...) }
