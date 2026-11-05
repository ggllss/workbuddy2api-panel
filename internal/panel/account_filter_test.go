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

// TestAccountReasonNoteTruncated 状态列的原因注脚必须单行省略，完整原因放 title。
//
// 背景：reason 是上游原文（如 "account banned by upstream (11140 request illegal),
// re-login required"），不限宽不换行时会把整行撑高，账号表被拉得很长。
func TestAccountReasonNoteTruncated(t *testing.T) {
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(js)
	if !strings.Contains(src, `class="acc-note" title="`) {
		t.Error("app.js 的原因注脚缺少 .acc-note 与 title（完整原因将无处可看）")
	}
	// 旧实现把样式写在行内，无法被 CSS 的单行省略覆盖——回归时这里会先炸。
	if strings.Contains(src, `class="hint" style="font-size:11.5px`) {
		t.Error("app.js 仍在用行内样式的 hint 注脚（应改用 .acc-note）")
	}

	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	h := string(html)
	for _, want := range []string{".acc .acc-note", "text-overflow: ellipsis", "white-space: nowrap"} {
		if !strings.Contains(h, want) {
			t.Errorf("index.html 的 .acc-note 缺少 %q（注脚不会省略成一行）", want)
		}
	}
}

// TestAutoGroupFrontendWiring 候选分组前端接线：一组一行 + 整组上下移/移除。
//
// 背景：分组是后端语义（auto.groups），但用户看到的必须是「一组一行」——
// 逐行展示会让同组通道散落在列表各处，看不出轮转关系。
func TestAutoGroupFrontendWiring(t *testing.T) {
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(js)
	for _, want := range []string{
		"function autoCandGrouped",     // 按身份稳定分组
		"function autoCandFlatGrouped", // 扁平化（保存用）
		"function autoCandMoveGroup",   // 整组上移/下移
		"function autoCandRemoveGroup", // 整组移除
		`data-act="gup"`,               // 组级按钮
		`data-act="gdown"`,
		`data-act="gdel"`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("app.js 缺少 %s（候选分组未接线）", want)
		}
	}
	// 身份判定必须剥掉 realm 前缀，否则同模型的不同域通道分不到一组。
	if !strings.Contains(src, "function autoCandIdentity") {
		t.Error("app.js 缺少 autoCandIdentity（去域同名判定）")
	}
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(html), `name="auto_groups"`) {
		t.Error("index.html 缺少 auto_groups 隐藏字段（分组不会随保存下发）")
	}
}
