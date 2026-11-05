// auto_groups_config_test.go 分组候选（auto.groups）的校验规则。
//
// 规则来自用户 2026-11-05 的明确要求：一个分组只能放同一个模型，判断标准是
// **去掉域后同名**——global:deepseek-v4.1-flash@cb 与 cn:deepseek-v4.1-flash@cb
// 同组；而 global:deepseek-v4.1-flash@cb 与 global:deepseek-v4.1-flash-sg@cb 不同组。
package main

import (
	"strings"
	"testing"
)

func TestNormalizeAutoGroups(t *testing.T) {
	ok, err := normalizeAutoGroups([][]string{
		{"global:deepseek-v4.1-flash@cb", "cn:deepseek-v4.1-flash@cb"},
		{"global:deepseek-v4.1-flash-sg@cb"},
	})
	if err != nil {
		t.Fatalf("合法分组被拒: %v", err)
	}
	if len(ok) != 2 || len(ok[0]) != 2 || ok[1][0] != "global:deepseek-v4.1-flash-sg@cb" {
		t.Fatalf("归一化结果不符: %v", ok)
	}

	bad := []struct {
		name string
		in   [][]string
		want string // 期望错误里出现的关键词
	}{
		{
			name: "组内不同模型",
			in:   [][]string{{"global:deepseek-v4.1-flash@cb", "cn:deepseek-v4.1-flash-sg@cb"}},
			want: "组内必须是同一个模型",
		},
		{
			name: "同一模型跨组",
			in: [][]string{
				{"global:deepseek-v4.1-flash@cb"},
				{"cn:deepseek-v4.1-flash@cb"},
			},
			want: "出现在多个分组",
		},
		{
			name: "缺 realm 前缀",
			in:   [][]string{{"deepseek-v4.1-flash@cb"}},
			want: "缺少 realm 前缀",
		},
		{
			name: "realm 非法",
			in:   [][]string{{"us:deepseek-v4.1-flash@cb"}},
			want: "realm 前缀非法",
		},
		{
			name: "自指",
			in:   [][]string{{"cn:auto:all"}},
			want: "自指",
		},
		{
			name: "空组",
			in:   [][]string{{}},
			want: "分组为空",
		},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			_, err := normalizeAutoGroups(tc.in)
			if err == nil {
				t.Fatalf("期望报错，实际通过: %v", tc.in)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息应含 %q，实际: %v", tc.want, err)
			}
		})
	}
}

// 组内重复元素去重（同一成员写两遍不算错，但只保留一次）。
func TestNormalizeAutoGroupsDedupeMember(t *testing.T) {
	got, err := normalizeAutoGroups([][]string{{
		"global:deepseek-v4.1-flash@cb",
		"global:deepseek-v4.1-flash@cb",
		"cn:deepseek-v4.1-flash@cb",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got[0]) != 2 {
		t.Fatalf("组内重复成员未去重: %v", got[0])
	}
}
