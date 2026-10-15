// auto_candidates_test.go 全量 auto（auto:all）候选列表在池层的存取测试：
// 默认空（功能关闭）、顺序原样保留、拷贝隔离、清空。
package pool

import (
	"reflect"
	"testing"
)

// TestAutoCandidatesDefaultOff 未注入 → nil（功能关闭，auto:all 由 handler 直接 400）。
func TestAutoCandidatesDefaultOff(t *testing.T) {
	p := New("")
	if got := p.AutoCandidates(); got != nil {
		t.Errorf("默认应为 nil（功能关闭），got %v", got)
	}
}

// TestAutoCandidatesRoundTrip 注入/读取保序（顺序即优先级），且两个方向都拷贝隔离：
// 调用方后续改动传入切片、或读取方改动返回值，都不影响池内状态。
func TestAutoCandidatesRoundTrip(t *testing.T) {
	p := New("")
	in := []string{"global:deepseek-v4.1-flash", "cn:deepseek-v4.1-flash"}
	p.SetAutoCandidates(in)
	want := []string{"global:deepseek-v4.1-flash", "cn:deepseek-v4.1-flash"}
	if got := p.AutoCandidates(); !reflect.DeepEqual(got, want) {
		t.Fatalf("AutoCandidates()=%v want %v", got, want)
	}

	in[0] = "cn:mutated"
	if got := p.AutoCandidates(); !reflect.DeepEqual(got, want) {
		t.Errorf("调用方改动传入切片影响了池内状态：%v", got)
	}

	got := p.AutoCandidates()
	got[1] = "cn:mutated"
	if again := p.AutoCandidates(); !reflect.DeepEqual(again, want) {
		t.Errorf("改动返回值影响了池内状态：%v", again)
	}
}

// TestAutoCandidatesClear 空切片/nil = 关闭该模式（面板清空候选后保存即此形态）。
func TestAutoCandidatesClear(t *testing.T) {
	p := New("")
	p.SetAutoCandidates([]string{"cn:a"})
	p.SetAutoCandidates([]string{})
	if got := p.AutoCandidates(); got != nil {
		t.Errorf("空切片应视为关闭（返回 nil），got %v", got)
	}
	p.SetAutoCandidates([]string{"cn:a"})
	p.SetAutoCandidates(nil)
	if got := p.AutoCandidates(); got != nil {
		t.Errorf("nil 应视为关闭（返回 nil），got %v", got)
	}
}
