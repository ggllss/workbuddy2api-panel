package pool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// SetAlias 设置别名：返回 true、内存 Status 透出、落盘可往返；别名未变化时幂等返回
// true 但**不写盘**（哨兵文件证明），否则面板每次失焦保存都会重写凭证文件。
func TestSetAliasPersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-u1.json")
	a := &auth.Auth{UID: "u1", Nickname: "上游昵称", AccessToken: "at", FilePath: fp}
	p := New("")
	p.Add(a)

	if !p.SetAlias("u1", "本地别名") {
		t.Fatal("设置别名应返回 true")
	}
	if st, _ := p.Status("u1"); st.Alias != "本地别名" {
		t.Fatalf("Status 别名=%q, want 本地别名", st.Alias)
	}
	reloaded, err := auth.Parse(mustRead(t, fp))
	if err != nil || reloaded.Alias != "本地别名" {
		t.Fatalf("回写后别名=%q err=%v, want 本地别名", reloaded.Alias, err)
	}
	if reloaded.Nickname != "上游昵称" {
		t.Fatalf("设置别名不应动 nickname，得到 %q", reloaded.Nickname)
	}

	// 未变化：幂等 true 且不写盘——用哨兵内容替换文件，若发生写盘哨兵必被覆盖。
	if err := os.WriteFile(fp, []byte(`{"sentinel":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !p.SetAlias("u1", "本地别名") {
		t.Fatal("别名未变化应幂等返回 true（面板重复保存不算失败）")
	}
	if raw, _ := os.ReadFile(fp); !strings.Contains(string(raw), "sentinel") {
		t.Fatalf("别名未变化不应写盘:\n%s", raw)
	}
}

// SetAlias 清除与拒绝：空串是**合法入参 = 清除别名**（回落 nickname，文件里删键）；
// 空 uid / 未知 uid 返回 false（面板据此报 404）。
func TestSetAliasClearAndUnknown(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-u1.json")
	a := &auth.Auth{UID: "u1", Nickname: "上游昵称", Alias: "旧别名", AccessToken: "at", FilePath: fp}
	p := New("")
	p.Add(a)

	if !p.SetAlias("u1", "") {
		t.Fatal("空别名 = 清除，应返回 true")
	}
	if st, _ := p.Status("u1"); st.Alias != "" {
		t.Fatalf("清除后 Status 别名=%q, want 空", st.Alias)
	}
	raw := mustRead(t, fp)
	if strings.Contains(string(raw), "alias") {
		t.Fatalf("清除后文件仍残留 alias 键:\n%s", raw)
	}
	reloaded, err := auth.Parse(raw)
	if err != nil || reloaded.Alias != "" || reloaded.Nickname != "上游昵称" {
		t.Fatalf("清除后 parse: alias=%q nickname=%q err=%v", reloaded.Alias, reloaded.Nickname, err)
	}

	if p.SetAlias("", "x") {
		t.Fatal("空 uid 应返回 false")
	}
	if p.SetAlias("no-such", "x") {
		t.Fatal("未知 uid 应返回 false")
	}
}

// 上游昵称同步（syncNicknames → SetNickname）不得覆盖本地别名——这正是别名独立成
// 字段的原因；同时验证同步照常更新 nickname。
func TestSetNicknameKeepsAlias(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-u1.json")
	a := &auth.Auth{UID: "u1", Nickname: "旧昵称", Alias: "本地别名", AccessToken: "at", FilePath: fp}
	p := New("")
	p.Add(a)
	if !p.SetAlias("u1", "本地别名") {
		t.Fatal("设置别名应返回 true")
	}

	if !p.SetNickname("u1", "上游新昵称") {
		t.Fatal("昵称变化应返回 true")
	}
	reloaded, err := auth.Parse(mustRead(t, fp))
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Alias != "本地别名" {
		t.Fatalf("上游同步后别名=%q, want 本地别名（不得被 nickname 覆盖）", reloaded.Alias)
	}
	if reloaded.Nickname != "上游新昵称" {
		t.Fatalf("上游同步后 nickname=%q, want 上游新昵称", reloaded.Nickname)
	}
	st, _ := p.Status("u1")
	if st.Alias != "本地别名" || st.Nickname != "上游新昵称" {
		t.Fatalf("Status alias=%q nickname=%q", st.Alias, st.Nickname)
	}
}
