// auto_group_rotate_test.go 组内轮转：同一模型的不同域通道应均匀分担流量。
package server

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestAutoCandidateOrderRotatesWithinGroup(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetAutoGroups([][]string{
		{"global:A", "cn:A"},
		{"global:B"},
	})
	h := NewHandler(Config{Pool: p})

	// 连续多次取顺序：A 组两个成员的首位应交替出现，B 组恒为自身。
	first := map[string]int{}
	for i := 0; i < 8; i++ {
		order := h.autoCandidateOrder()
		if len(order) != 3 {
			t.Fatalf("候选数应为 3（A 组 2 + B 组 1），实际 %d: %v", len(order), order)
		}
		// 组内必须相邻（扁平列表保持组块，降级判定依赖这一点）。
		if order[2] != "global:B" {
			t.Fatalf("组序被打乱，B 组应在最后: %v", order)
		}
		if order[0] != "global:A" && order[0] != "cn:A" {
			t.Fatalf("A 组首位应是其成员之一: %v", order)
		}
		first[order[0]]++
	}
	// 轮转生效：两个成员都当过首位（否则「均匀分配」名存实亡）。
	if first["global:A"] == 0 || first["cn:A"] == 0 {
		t.Fatalf("组内未轮转，首位分布: %v", first)
	}
}

// 未配置分组时退回扁平候选列表（旧配置零回归）。
func TestAutoCandidateOrderFallsBackToCandidates(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetAutoCandidates([]string{"global:A", "global:B"})
	h := NewHandler(Config{Pool: p})
	got := h.autoCandidateOrder()
	if len(got) != 2 || got[0] != "global:A" || got[1] != "global:B" {
		t.Fatalf("未配置分组时应原样返回候选列表，实际: %v", got)
	}
}
