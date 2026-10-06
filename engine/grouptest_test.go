package engine

import (
	"fmt"
	"os"
	"sort"
	"testing"
)

// 轻量分组用例框架：既走 testing 子用例（go test -v 可见单条），
// 又在汇总输出里按业务分组打印用例数与通过/失败结果，与上一轮的交付风格保持一致。

type groupCase struct {
	group string
	name  string
	fn    func(t *testing.T)
}

var registered []groupCase

func register(group, name string, fn func(t *testing.T)) {
	registered = append(registered, groupCase{group: group, name: name, fn: fn})
}

func TestGroups(t *testing.T) {
	groups := map[string][]groupCase{}
	var order []string
	for _, c := range registered {
		if _, ok := groups[c.group]; !ok {
			order = append(order, c.group)
		}
		groups[c.group] = append(groups[c.group], c)
	}
	sort.Strings(order)

	type stat struct {
		pass, fail int
		names      []string
	}
	stats := map[string]*stat{}
	for _, g := range order {
		stats[g] = &stat{}
		for _, c := range groups[g] {
			c := c
			ok := t.Run(g+"/"+c.name, func(t *testing.T) {
				c.fn(t)
			})
			if ok {
				stats[g].pass++
			} else {
				stats[g].fail++
			}
			stats[g].names = append(stats[g].names, c.name)
		}
	}

	fmt.Println()
	fmt.Println("================ 业务分组测试结果 ================")
	var totalPass, totalFail int
	for _, g := range order {
		s := stats[g]
		totalPass += s.pass
		totalFail += s.fail
		result := "PASS"
		if s.fail > 0 {
			result = "FAIL"
		}
		fmt.Printf("[%-10s] 分组=%-24s 用例数=%-3d 通过=%-3d 失败=%-3d\n", result, g, s.pass+s.fail, s.pass, s.fail)
	}
	fmt.Printf("[%-10s] 合计用例=%d 通过=%d 失败=%d\n", map[bool]string{true: "PASS", false: "FAIL"}[totalFail == 0], totalPass+totalFail, totalPass, totalFail)
	fmt.Println("==================================================")
	fmt.Println()
	if totalFail > 0 {
		os.Exit(1)
	}
}
