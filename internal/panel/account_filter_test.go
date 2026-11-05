// account_filter_test.go 账号池筛选与按域排序的前端接线钉子。
//
// 为什么需要：账号池的筛选/排序全在 app.js 里做（纯前端，不打上游）。前端是
// go:embed 资源，Go 编译器不校验它——若哪天重构把控件 id 或排序逻辑删了，
// 后端测试依然全绿，而面板上的筛选条会变成一个按了没反应的摆设。
package panel

import (
	"os"
	"strings"
	"testing"
)

func TestAccountFilterFrontendWiring(t *testing.T) {
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(js)
	// 三个筛选维度 + 重置 + 计数提示，缺一个筛选条就不完整。
	for _, want := range []string{
		"accRealm", "accStatus", "accLock", // 三个下拉
		"accFilterReset",         // 重置
		"accFilterNote",          // 「筛选后 N / M」
		"function accStatusOf",   // 状态口径（与状态标签同源）
		"function renderAccBody", // 筛选+排序+渲染
	} {
		if !strings.Contains(src, want) {
			t.Errorf("app.js 缺少 %s（账号池筛选未接线）", want)
		}
	}
	// 默认按域排序：比较器必须按 global 归组，否则「默认按域排序」名存实亡。
	if !strings.Contains(src, "a.realm === 'global' ? 1 : 0") {
		t.Error("app.js 缺少按域排序比较器（默认排序失效）")
	}
	// 进度条基准必须取全池：若误改成筛选后的集合，勾一个筛选条长就会跳一次。
	if !strings.Contains(src, "...accAll.map(s => s.credits || 0)") {
		t.Error("进度条基准应取 accAll（全池），不随筛选缩放")
	}

	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	h := string(html)
	for _, want := range []string{`id="accRealm"`, `id="accStatus"`, `id="accLock"`, `id="accFilterReset"`} {
		if !strings.Contains(h, want) {
			t.Errorf("index.html 缺少 %s（筛选条未渲染）", want)
		}
	}
	// 状态选项必须覆盖 accStatusOf 的全部返回值，否则某类账号筛不出来。
	for _, v := range []string{`value="ok"`, `value="cool"`, `value="off"`, `value="paused"`} {
		if !strings.Contains(h, v) {
			t.Errorf("index.html 状态筛选缺少选项 %s", v)
		}
	}
}
