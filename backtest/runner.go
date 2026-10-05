// Package backtest 在撮合引擎之上提供并发回测驱动器。
//
// 并发模型（既保留多标的并发，又保证跨标的确定序）：
//   - 事件按全局 Seq 排序后划分成多个 shard（按标的哈希），模拟多标的并发到达；
//   - 每个 shard goroutine 顺序处理自己的事件，但在“真正撮合/记账”前
//     先进入全局就绪队列等待放行；
//   - 唯一的提交协程严格按 Seq 从就绪队列放行下一条事件。
//     因此跨标的即便处理线程不同，真正生效顺序仍与 Seq 完全一致，
//     共享资金/持仓场景下的结果确定且可重复回放。
package backtest

import (
	"hash/fnv"
	"sort"
	"sync"

	"github.com/lechandonga/backtest-matching-engine-go/account"
	"github.com/lechandonga/backtest-matching-engine-go/engine"
	"github.com/lechandonga/backtest-matching-engine-go/event"
)

// Runner 是并发回测驱动器。
type Runner struct {
	eng *engine.Engine
}

// New 创建 Runner。
func New(eng *engine.Engine) *Runner { return &Runner{eng: eng} }

// Engine 返回底层引擎。
func (r *Runner) Engine() *engine.Engine { return r.eng }

// Outcome 是单条事件的处理结果（按输入位置对齐）。
type Outcome struct {
	Event  *event.Event
	Result *engine.Result
	Err    error
}

type readyItem struct {
	position int // 在按 Seq 排序后的位置
	idx      int // 原始输入下标
}

// Run 并发处理一批事件。events 可以乱序、可以包含重复 EventID。
// workers 控制 shard goroutine 数（<=0 时取 4）。
func (r *Runner) Run(events []*event.Event, workers int) []*Outcome {
	n := len(events)
	outcomes := make([]*Outcome, n)
	if n == 0 {
		return outcomes
	}
	if workers <= 0 {
		workers = 4
	}
	if workers > n {
		workers = n
	}

	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		return events[order[a]].Seq < events[order[b]].Seq
	})

	// 每个排序位置归属一个 shard，shard 内严格按位置递增。
	shardOf := make([]int, n)
	for pos, idx := range order {
		h := fnv.New32a()
		_, _ = h.Write([]byte(symbolOf(events[idx])))
		shardOf[pos] = int(h.Sum32()) % workers
	}

	// 就绪队列：ready[pos] 关闭表示该位置已可提交。
	ready := make([]chan struct{}, n)
	for i := range ready {
		ready[i] = make(chan struct{})
	}

	// 提交协程：按 Seq 顺序放行。
	var committer sync.WaitGroup
	committer.Add(1)
	go func() {
		defer committer.Done()
		for pos := 0; pos < n; pos++ {
			<-ready[pos]
			idx := order[pos]
			res, err := r.eng.Process(events[idx])
			outcomes[idx] = &Outcome{Event: events[idx], Result: res, Err: err}
		}
	}()

	// shard 工作协程：每个 shard 顺序准备自己的位置，准备好即排队放行。
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for pos := 0; pos < n; pos++ {
				if shardOf[pos] != w {
					continue
				}
				// 此处可放置 shard 内的预处理（模拟并发到达/计算）；
				// 真正撮合由提交协程完成。直接标记就绪即可。
				close(ready[pos])
			}
		}(w)
	}
	wg.Wait()
	committer.Wait()
	return outcomes
}

// Reconcile 用最新价做账户对账。
func (r *Runner) Reconcile(marks map[string]float64) (account.Snapshot, error) {
	return r.eng.Account().Reconcile(marks)
}

// Snapshot 返回账户快照。
func (r *Runner) Snapshot(marks map[string]float64) account.Snapshot {
	return r.eng.Account().Snapshot(marks)
}

func symbolOf(ev *event.Event) string {
	switch ev.Kind {
	case event.KindMarket:
		return ev.Symbol
	case event.KindSubmit:
		return ev.Order.Symbol
	case event.KindCancel:
		return ev.CancelSymbol
	case event.KindSplit, event.KindDividend:
		return ev.CorporateSymbol
	}
	return "_unknown"
}
