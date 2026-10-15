package auth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// alias 往返（嵌套形）：account.alias 必须被 Parse 读出，且与 nickname 并存互不干扰。
func TestParseAliasNested(t *testing.T) {
	raw := []byte(`{"auth":{"accessToken":"at"},"account":{"uid":"u1","nickname":"上游昵称","alias":"本地别名"}}`)
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("nested parse err: %v", err)
	}
	if a.Alias != "本地别名" {
		t.Errorf("nested alias=%q, want 本地别名", a.Alias)
	}
	if a.Nickname != "上游昵称" {
		t.Errorf("nested nickname=%q, want 上游昵称（别名不得顶掉昵称）", a.Nickname)
	}
}

// alias 往返（扁平形）：手写文件的顶层 alias（与 uid/nickname 同层）也要能读。
func TestParseAliasFlat(t *testing.T) {
	raw := []byte(`{"accessToken":"at","uid":"u2","nickname":"n2","alias":"别名2"}`)
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("flat parse err: %v", err)
	}
	if a.Alias != "别名2" || a.Nickname != "n2" {
		t.Errorf("flat alias=%q nickname=%q, want 别名2 / n2", a.Alias, a.Nickname)
	}
}

// 旧文件（从未有过 alias 键）必须照常解析：Alias 为空串，不报错、不改变其它字段。
func TestParseLegacyWithoutAlias(t *testing.T) {
	nested := []byte(`{"auth":{"accessToken":"at","refreshToken":"rt"},"account":{"uid":"u1","nickname":"n1"}}`)
	a, err := Parse(nested)
	if err != nil || a.Alias != "" || a.Nickname != "n1" || a.UID != "u1" {
		t.Fatalf("旧嵌套形: alias=%q nickname=%q uid=%q err=%v", a.Alias, a.Nickname, a.UID, err)
	}
	flat := []byte(`{"accessToken":"at","uid":"u2","nickname":"n2"}`)
	b, err := Parse(flat)
	if err != nil || b.Alias != "" || b.Nickname != "n2" {
		t.Fatalf("旧扁平形: alias=%q nickname=%q err=%v", b.Alias, b.Nickname, err)
	}
}

// SaveAtomic 必须把别名写进白名单（account.alias）并往返无损——白名单全量重写，
// 漏加一个键就是「面板保存成功但重启后别名消失」。
func TestSaveAtomicAliasRoundtrip(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-u1.json")
	a := &Auth{AccessToken: "at", UID: "u1", Nickname: "n1", Alias: "本地别名", FilePath: fp}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"alias": "本地别名"`) {
		t.Fatalf("落盘文件缺 account.alias:\n%s", raw)
	}
	b, err := Parse(raw)
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if b.Alias != "本地别名" || b.Nickname != "n1" || b.UID != "u1" {
		t.Errorf("roundtrip: alias=%q nickname=%q uid=%q", b.Alias, b.Nickname, b.UID)
	}
}

// LocalAliasFromFile 是「凭证重写前继承本地别名」的唯一入口（面板重登 / cockpit 导入 /
// login.sh 三处调用）。既有文件不存在（新 uid）、损坏、无 alias 键、path 为空时都必须
// 返回空串且不报错——继承是尽力而为，坏文件绝不能阻塞重登落盘。
func TestLocalAliasFromFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := []struct {
		name, path, want string
	}{
		{"嵌套形 account.alias",
			write("nested.json", `{"auth":{"accessToken":"at"},"account":{"uid":"u1","nickname":"n1","alias":"本地别名"}}`),
			"本地别名"},
		{"扁平形顶层 alias",
			write("flat.json", `{"accessToken":"at","uid":"u2","alias":"扁平别名"}`),
			"扁平别名"},
		{"无 alias 键（旧文件）",
			write("noalias.json", `{"auth":{"accessToken":"at"},"account":{"uid":"u3","nickname":"n3"}}`),
			""},
		{"JSON 损坏", write("broken.json", `{"auth":`), ""},
		// JSON 合法但类型不对：Parse 一律报错 → 空串（与 login.sh 同口径：只跳过继承，
		// 绝不阻塞写入）。这几组与 login.sh 内联 Python 的健壮性用例一一对应。
		{"account 是字符串", write("t1.json", `{"account":"oops"}`), ""},
		{"顶层是数组", write("t2.json", `[]`), ""},
		{"顶层是字符串", write("t3.json", `"oops"`), ""},
		{"account 是数组", write("t4.json", `{"account":["a"]}`), ""},
		{"alias 是数字", write("t5.json", `{"account":{"alias":123},"auth":{"accessToken":"at"}}`), ""},
		{"文件不存在（新 uid）", filepath.Join(dir, "nope.json"), ""},
		{"path 为空", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := LocalAliasFromFile(c.path); got != c.want {
				t.Fatalf("LocalAliasFromFile=%q, want %q", got, c.want)
			}
		})
	}
}

// 空别名不落键：从未设过别名的文件不得被引入空 alias 键；清除别名后该键应被删除
// （回到旧文件形状），Parse 读回空串 = 往返无损。
func TestSaveAtomicOmitsEmptyAlias(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-u1.json")
	a := &Auth{AccessToken: "at", UID: "u1", Nickname: "n1", FilePath: fp}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, _ := os.ReadFile(fp)
	if strings.Contains(string(raw), "alias") {
		t.Fatalf("空别名不应写键:\n%s", raw)
	}

	a.Alias = "临时别名"
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("save with alias: %v", err)
	}
	raw, _ = os.ReadFile(fp)
	if !strings.Contains(string(raw), "临时别名") {
		t.Fatalf("别名未落盘:\n%s", raw)
	}

	a.Alias = ""
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("save after clear: %v", err)
	}
	raw, _ = os.ReadFile(fp)
	if strings.Contains(string(raw), "alias") {
		t.Fatalf("清除别名后文件仍残留 alias 键:\n%s", raw)
	}
	b, err := Parse(raw)
	if err != nil || b.Alias != "" || b.Nickname != "n1" {
		t.Fatalf("清除后 parse: alias=%q nickname=%q err=%v", b.Alias, b.Nickname, err)
	}
}
