package panel

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// newAliasPanel 建一个带单账号（uid=u1，有上游昵称）的面板，返回面板、池与凭证文件路径。
func newAliasPanel(t *testing.T) (*Panel, *pool.Pool, string) {
	t.Helper()
	fp := filepath.Join(t.TempDir(), "workbuddy-u1.json")
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", Nickname: "上游昵称", AccessToken: "at", FilePath: fp})
	return New(Config{Version: "test", APIKey: "k", Pool: p}), p, fp
}

func postAlias(t *testing.T, p *Panel, uid, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/panel/api/accounts/"+uid+"/alias", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer k")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	return rr
}

// 设置别名 → 200 {ok:true}，池状态与凭证文件同时生效；空别名 → 200 清除并回落昵称。
func TestAccountAliasSetThenClear(t *testing.T) {
	p, pl, fp := newAliasPanel(t)

	rr := postAlias(t, p, "u1", `{"alias":"本地别名"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("设置别名 -> %d %s", rr.Code, rr.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil || out["ok"] != true {
		t.Fatalf("响应=%s err=%v, want {ok:true}", rr.Body, err)
	}
	if st, _ := pl.Status("u1"); st.Alias != "本地别名" {
		t.Fatalf("池内别名=%q, want 本地别名", st.Alias)
	}
	raw, _ := os.ReadFile(fp)
	if !strings.Contains(string(raw), "本地别名") {
		t.Fatalf("别名未落盘:\n%s", raw)
	}

	rr = postAlias(t, p, "u1", `{"alias":""}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("清除别名 -> %d %s", rr.Code, rr.Body)
	}
	st, _ := pl.Status("u1")
	if st.Alias != "" {
		t.Fatalf("清除后别名=%q, want 空", st.Alias)
	}
	if st.Nickname != "上游昵称" {
		t.Fatalf("清除别名后昵称=%q, want 上游昵称（回落展示）", st.Nickname)
	}
	if raw, _ := os.ReadFile(fp); strings.Contains(string(raw), "本地别名") {
		t.Fatalf("清除后文件仍残留别名:\n%s", raw)
	}
}

// 前端接线钉子：app.js 必须真的指向 /alias 路由并带 data-edit="alias" 标记，否则
// 后端全绿而面板点名字毫无反应（前端是 go:embed 资源，Go 编译器不校验）。
func TestAliasFrontendWiring(t *testing.T) {
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"'/alias'", `data-edit="alias"`, "data-alias="} {
		if !strings.Contains(string(js), want) {
			t.Errorf("app.js 缺少 %s（别名交互未接线）", want)
		}
	}
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(html), ".alias-input") {
		t.Error("index.html 缺少 .alias-input 样式")
	}
}

// 未知 uid → 404（沿用 writeErr 的 account not found 口径）。
func TestAccountAliasNotFound(t *testing.T) {
	p, _, _ := newAliasPanel(t)
	rr := postAlias(t, p, "ghost", `{"alias":"x"}`)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("未知 uid -> %d %s, want 404", rr.Code, rr.Body)
	}
	if !strings.Contains(rr.Body.String(), "account not found") {
		t.Fatalf("404 响应体=%s", rr.Body)
	}
}

// 入参校验：全空白拒绝（空串才是清除）、显示宽度超 32（CJK 计 2）拒绝、坏 JSON 拒绝；
// 边界值 32 显示宽度（16 个汉字）必须放行——否则限制比面板文案更严。
func TestAccountAliasValidation(t *testing.T) {
	p, pl, _ := newAliasPanel(t)
	cases := []struct {
		name, body string
		want       int
	}{
		{"全空白", `{"alias":"   "}`, http.StatusBadRequest},
		{"坏 JSON", `{"alias":`, http.StatusBadRequest},
		{"超宽 17 个汉字 = 34 列", `{"alias":"` + strings.Repeat("字", 17) + `"}`, http.StatusBadRequest},
		{"边界 16 个汉字 = 32 列", `{"alias":"` + strings.Repeat("字", 16) + `"}`, http.StatusOK},
		{"前后空白裁掉后合法", `{"alias":"  ab  "}`, http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if rr := postAlias(t, p, "u1", c.body); rr.Code != c.want {
				t.Fatalf("-> %d %s, want %d", rr.Code, rr.Body, c.want)
			}
		})
	}
	if st, _ := pl.Status("u1"); st.Alias != "ab" {
		t.Fatalf("末次请求后别名=%q, want ab（前后空白应被裁掉）", st.Alias)
	}
}

// ── 凭证重写不丢别名 ────────────────────────────────────────────────────────
// 面板重登 / cockpit 导入都会用全新 Auth 覆盖同一 uid 的凭证文件；别名是本地字段，
// 必须被继承下来（否则用户设的别名会在一次常规重登后凭空消失）。

// 面板重登路径（loginPoll → newLoginAuth）：既有文件有别名 → 继承；凭证字段以本次登录
// 为准；新 uid（无既有文件）不引入空 alias 键。
func TestNewLoginAuthKeepsAlias(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-u1.json")
	old := &auth.Auth{UID: "u1", Nickname: "旧昵称", Alias: "本地别名", AccessToken: "old", FilePath: fp}
	if err := old.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	p := New(Config{Version: "test", APIKey: "k", Pool: pool.New(""), AuthDir: dir})

	a := p.newLoginAuth(loginToken{AccessToken: "new", RefreshToken: "rt", ExpiresIn: 3600},
		loginAccount{UID: "u1", Nickname: "新昵称"})
	if a.Alias != "本地别名" {
		t.Fatalf("重登后别名=%q, want 本地别名（不得被凭证重写抹掉）", a.Alias)
	}
	if a.AccessToken != "new" || a.Nickname != "新昵称" {
		t.Fatalf("凭证/昵称必须取本次登录：token=%q nickname=%q", a.AccessToken, a.Nickname)
	}

	b := p.newLoginAuth(loginToken{AccessToken: "new2", ExpiresIn: 3600}, loginAccount{UID: "u2"})
	if b.Alias != "" {
		t.Fatalf("新 uid 别名=%q, want 空", b.Alias)
	}
	if err := b.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(b.FilePath); strings.Contains(string(raw), "alias") {
		t.Fatalf("新 uid 不应引入空 alias 键:\n%s", raw)
	}
}

// cockpit 导入路径（importCockpit 端到端）：覆盖既有 uid 的凭证时别名保留、凭证以导入
// 为准；新 uid 不引入空 alias 键。
func TestImportCockpitKeepsAlias(t *testing.T) {
	dir := t.TempDir()
	p, _, _ := newImportPanel(t, dir)
	fp := filepath.Join(dir, "workbuddy-u1.json")
	old := &auth.Auth{UID: "u1", Nickname: "旧昵称", Alias: "本地别名", AccessToken: "old", FilePath: fp}
	if err := old.SaveAtomic(); err != nil {
		t.Fatal(err)
	}

	body := `[{"uid":"u1","access_token":"at2","refresh_token":"rt2","domain":"","nickname":"导入昵称"},
	          {"uid":"u2","access_token":"at3","refresh_token":"rt3","domain":"","nickname":"新账号"}]`
	rr := postCockpit(t, p, body)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"imported":2`) {
		t.Fatalf("导入 -> %d %s", rr.Code, rr.Body)
	}

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	got, err := auth.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.Alias != "本地别名" {
		t.Fatalf("导入覆盖后别名=%q, want 本地别名", got.Alias)
	}
	if got.AccessToken != "at2" || got.Nickname != "导入昵称" {
		t.Fatalf("凭证必须取本次导入：token=%q nickname=%q", got.AccessToken, got.Nickname)
	}

	raw2, err := os.ReadFile(filepath.Join(dir, "workbuddy-u2.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw2), "alias") {
		t.Fatalf("新 uid 不应引入空 alias 键:\n%s", raw2)
	}
}

// newImportPanel 建面板 + 假上游（一律返回业务错误信封）：导入落盘后还会顺带签到/查资源，
// 这些调用失败只记日志，不影响文件内容。返回 code!=0 而非 5xx——5xx 会触发上游的瞬时错误
// 重试（间隔 2s+4s），把这条单测拖成二十多秒。
func newImportPanel(t *testing.T, authDir string) (*Panel, *pool.Pool, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":40001,"msg":"stub"}`))
	}))
	t.Cleanup(srv.Close)
	up := upstream.New()
	up.ChatBaseCN, up.BillingBaseCN, up.WebBaseCN = srv.URL, srv.URL, srv.URL
	up.ChatBaseGlobal, up.BillingBaseGlobal = srv.URL, srv.URL
	pl := pool.New("")
	return New(Config{Version: "test", APIKey: "k", Pool: pl, AuthDir: authDir, Upstream: up}), pl, authDir
}

func postCockpit(t *testing.T, p *Panel, body string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "accounts.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/panel/api/import/cockpit", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer k")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	return rr
}
