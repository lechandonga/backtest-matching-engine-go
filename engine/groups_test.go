package engine

// 业务分组测试辅助：所有用例通过 group() 归组，测试结束时按业务分组打印
// 用例数与结果，风格与上一轮保持一致（命令行清晰可见每组 PASS/FAIL 计数）。

import (
	"fmt"
	"sort"
	"sync"
	"testing"
)

const (
	grpLegacy      = "01-legacy-compat"  // 上一轮既有行为不回归
	grpTrigger     = "02-trigger-orders" // 止损/止损限价/跟踪止损
	grpTIF         = "03-time-in-force"  // DAY/IOC/FOK/GTC
	grpDayBoundary = "04-trading-day"    // 收盘、重复收盘、跨日
	grpReplay      = "05-replay-perf"    // 大回放稳定性/耗时/内存
)

var (
	groupMu    sync.Mutex
	groupStats = map[string]struct{ pass, fail int }{}
)

func record(group string, ok bool) {
	groupMu.Lock()
	defer groupMu.Unlock()
	s := groupStats[group]
	if ok {
		s.pass++
	} else {
		s.fail++
	}
	groupStats[group] = s
}

func group(t *testing.T, name string) func(bool) {
	t.Helper()
	return func(ok bool) {
		t.Helper()
		record(name, ok)
		if !ok {
			t.Errorf("[%s] assertion failed", name)
		}
	}
}

func TestMain(m *testing.M) {
	m.Run()

	groups := make([]string, 0, len(groupStats))
	for g := range groupStats {
		groups = append(groups, g)
	}
	sort.Strings(groups)

	fmt.Println("\n================ 业务分组用例统计 ================")
	var totalPass, totalFail int
	for _, g := range groups {
		s := groupStats[g]
		status := "PASS"
		if s.fail > 0 {
			status = "FAIL"
		}
		fmt.Printf("  %-24s cases=%-3d pass=%-3d fail=%-3d [%s]\n",
			g, s.pass+s.fail, s.pass, s.fail, status)
		totalPass += s.pass
		totalFail += s.fail
	}
	fmt.Printf("  %-24s cases=%-3d pass=%-3d fail=%-3d\n",
		"TOTAL", totalPass+totalFail, totalPass, totalFail)
	fmt.Println("=================================================")
	fmt.Println("RESULT: see group table above")
}
