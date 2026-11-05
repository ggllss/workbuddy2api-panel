// auto_group_rotate_test.go 组内加权轮转：按**账号数量**分流量，不是按域平分。
//
// 需求（用户 2026-11-05 纠正）：cn 1 个号 + global 4 个号时应该是 1:4，
// 让每个账号吃到的流量相等；global 掉一个号自动变 1:3。
// 按成员均分会让 1 个号的域和 4 个号的域各吃 50%，等于 CN 那一个号扛 5 个号的量。
package server

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestWeightedCycleRatios 轮转表里各成员出现次数 = 其权重。
func TestWeightedCycleRatios(t *testing.T) {
	cases := []struct {
		name    string
		weights []int
		want    []int // 每个成员出现的次数
	}{
		{"cn1:glo4", []int{1, 4}, []int{1, 4}},
		{"cn1:glo3（掉一个号）", []int{1, 3}, []int{1, 3}},
		{"cn1:glo1", []int{1, 1}, []int{1, 1}},
		{"三域 1:2:3", []int{1, 2, 3}, []int{1, 2, 3}},
		{"某成员无号", []int{0, 4}, []int{0, 4}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cycle := weightedCycle(tc.weights)
			got := make([]int, len(tc.weights))
			for _, idx := range cycle {
				got[idx]++
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("成员 %d 出现 %d 次，want %d（全表 %v）", i, got[i], tc.want[i], cycle)
				}
			}
		})
	}
}

// TestWeightedCycleSmooth 权重 1:4 必须**交错**排布，不能前四个全给 global
// （否则连发 4 次都落同一个域，等于没轮转）。
func TestWeightedCycleSmooth(t *testing.T) {
	cycle := weightedCycle([]int{1, 4})
	if len(cycle) != 5 {
		t.Fatalf("表长应为 5，实际 %d", len(cycle))
	}
	// 权重 1 的成员（索引 0）必须出现在前半段，不能挤在最后。
	pos := -1
	for i, idx := range cycle {
		if idx == 0 {
			pos = i
			break
		}
	}
	if pos < 0 {
		t.Fatalf("权重 1 的成员在整张表里没出现: %v", cycle)
	}
	if pos > 2 {
		t.Fatalf("权重 1 的成员位次过于靠后（第 %d 位），未交错: %v", pos+1, cycle)
	}
}

// 注：Auth.realm 是私有字段，这里用 Domain 后缀（isGlobalDomain）构造 global 账号——
// 与生产凭证的判定路径一致，比直接塞私有字段更接近真实。
//
// TestAutoCandidateOrderWeightedByAccounts 1 个 cn 号 + 4 个 global 号 →
// 5 次请求里 cn 只在其中 1 次排首位。
func TestAutoCandidateOrderWeightedByAccounts(t *testing.T) {
	auths := []*auth.Auth{{UID: "cn1", AccessToken: "at", Domain: "www.codebuddy.cn", ExpiresAt: 9999999999}}
	for _, uid := range []string{"g1", "g2", "g3", "g4"} {
		auths = append(auths, &auth.Auth{UID: uid, AccessToken: "at", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999})
	}
	p := testPoolWith(auths...)
	p.SetAutoGroups([][]string{{"global:A", "cn:A"}})
	h := NewHandler(Config{Pool: p})

	first := map[string]int{}
	for i := 0; i < 5; i++ {
		order := h.autoCandidateOrder()
		if len(order) != 2 {
			t.Fatalf("候选数应为 2，实际 %d: %v", len(order), order)
		}
		first[order[0]]++
	}
	if first["cn:A"] != 1 || first["global:A"] != 4 {
		t.Fatalf("1:4 加权不成立，5 次里首位分布 %v（期望 cn:A=1, global:A=4）", first)
	}
}

// TestAutoCandidateOrderWeightedAfterAccountLoss global 只剩 3 个号 → 自动变 1:3。
func TestAutoCandidateOrderWeightedAfterAccountLoss(t *testing.T) {
	auths := []*auth.Auth{{UID: "cn1", AccessToken: "at", Domain: "www.codebuddy.cn", ExpiresAt: 9999999999}}
	for _, uid := range []string{"g1", "g2", "g3"} {
		auths = append(auths, &auth.Auth{UID: uid, AccessToken: "at", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999})
	}
	p := testPoolWith(auths...)
	p.SetAutoGroups([][]string{{"global:A", "cn:A"}})
	h := NewHandler(Config{Pool: p})

	first := map[string]int{}
	for i := 0; i < 4; i++ {
		first[h.autoCandidateOrder()[0]]++
	}
	if first["cn:A"] != 1 || first["global:A"] != 3 {
		t.Fatalf("掉一个 global 号后应为 1:3，实际首位分布 %v", first)
	}
}

// TestAutoCandidateOrderFallsBackWhenGroupEmpty 全组无号 → 保持配置顺序，
// 让上层按既有判据降级到下一组。
func TestAutoCandidateOrderFallsBackWhenGroupEmpty(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", Domain: "www.codebuddy.cn", ExpiresAt: 9999999999})
	p.SetAutoGroups([][]string{{"global:A", "global:B"}}) // 没有 global 号
	h := NewHandler(Config{Pool: p})
	got := h.autoCandidateOrder()
	if len(got) != 2 || got[0] != "global:A" || got[1] != "global:B" {
		t.Fatalf("全组无号时应保持配置顺序，实际: %v", got)
	}
}

// TestAutoCandidateOrderFallsBackToCandidates 未配置分组时退回扁平候选列表。
func TestAutoCandidateOrderFallsBackToCandidates(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetAutoCandidates([]string{"global:A", "global:B"})
	h := NewHandler(Config{Pool: p})
	got := h.autoCandidateOrder()
	if len(got) != 2 || got[0] != "global:A" || got[1] != "global:B" {
		t.Fatalf("未配置分组时应原样返回候选列表，实际: %v", got)
	}
}
