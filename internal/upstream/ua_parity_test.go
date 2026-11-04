package upstream

// ua_parity_test.go 出站 UA / 客户端版本号与**官方真机抓包**的一致性钉子。
//
// 官方事实（2026-10-30 官方分发包拆包 + 真机抓包，见 headers.go 常量注释）：
//
//	CN      /v2/chat/completions UA = WorkBuddy/5.9.3 WorkBuddy/5.9.3 CLI/2.165.0
//	global  /v2/chat/completions UA = WorkBuddy/5.9.4 workbuddy-ai/5.9.4 CLI/2.165.0
//	两者 X-IDE-Version 均等于 UA 的版本段；UA 恒为 3 段。
//
// 这些断言是「非官方客户端识别」防线：UA 结构（段数）、品牌 slug（有无空格）、
// 版本段与 X-IDE-Version 的自洽，任一处偏差都是静态规则一命就中的特征。

import (
	"net/http"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const (
	officialUACN     = "WorkBuddy/5.9.3 WorkBuddy/5.9.3 CLI/2.165.0"
	officialUAGlobal = "WorkBuddy/5.9.4 workbuddy-ai/5.9.4 CLI/2.165.0"
)

// cnAuth / globalAuth 构造两个 realm 的账号（BackfillRealm 按 Domain 定 realm）。
func cnAuth() *auth.Auth {
	a := &auth.Auth{UID: "u-cn", AccessToken: "at", Domain: "copilot.tencent.com"}
	a.BackfillRealm()
	return a
}

func globalAuth() *auth.Auth {
	a := &auth.Auth{UID: "u-gl", AccessToken: "at", Domain: "www.workbuddy.ai"}
	a.BackfillRealm()
	return a
}

// TestUAParityCNExact CN 账号默认 UA 逐字符等于官方真机串（驼峰品牌段）。
func TestUAParityCNExact(t *testing.T) {
	c := New()
	got := c.defaultWorkBuddyUAFor(cnAuth())
	t.Logf("CN 实际 UA = %q（X-IDE-Version = %q）", got, c.clientVersionFor(cnAuth()))
	if got != officialUACN {
		t.Errorf("CN UA = %q want %q", got, officialUACN)
	}
}

// TestUAParityGlobalExact global 账号默认 UA 逐字符等于官方真机串
// （第二段是小写连字符 slug `workbuddy-ai`，不是 productName `WorkBuddy AI`）。
func TestUAParityGlobalExact(t *testing.T) {
	c := New()
	got := c.defaultWorkBuddyUAFor(globalAuth())
	t.Logf("global 实际 UA = %q（X-IDE-Version = %q）", got, c.clientVersionFor(globalAuth()))
	if got != officialUAGlobal {
		t.Errorf("global UA = %q want %q", got, officialUAGlobal)
	}
}

// TestUAParityThreeFields UA 必须**恰好 3 段**（strings.Fields 按空格切）。
//
// 这是对旧 bug 的钉子：旧实现把 global 第二段写成 `WorkBuddy AI`（带空格），
// 实际产出 4 段 `WorkBuddy/5.5.4 WorkBuddy AI/5.5.4 CLI/2.137.1`——官方是 3 段，
// 段数不同就是最容易被静态规则命中的「非官方客户端」形态。
func TestUAParityThreeFields(t *testing.T) {
	c := New()
	for _, tc := range []struct {
		name string
		a    *auth.Auth
		want string
	}{
		{"cn", cnAuth(), officialUACN},
		{"global", globalAuth(), officialUAGlobal},
	} {
		ua := c.defaultWorkBuddyUAFor(tc.a)
		if n := len(strings.Fields(ua)); n != 3 {
			t.Errorf("%s UA = %q fields=%d want 3", tc.name, ua, n)
		}
		// 品牌段里绝不能出现空格（否则段数就变了）。
		for _, f := range strings.Fields(ua) {
			if strings.ContainsAny(f, " ") {
				t.Errorf("%s UA 段 %q 含空格", tc.name, f)
			}
		}
		if ua != tc.want {
			t.Errorf("%s UA = %q want %q", tc.name, ua, tc.want)
		}
	}
}

// TestUAParityVersionMatchesIDEVersion 同一请求内自洽：UA 的版本段 == X-IDE-Version。
//
// 旧实现的自相矛盾点：desktop.go 硬编码 UA 说 5.5.6，而 X-IDE-Version 走
// defaultClientVersion 说 5.5.4——同一进程两个版本号，本身就是可判别特征。
func TestUAParityVersionMatchesIDEVersion(t *testing.T) {
	for _, tc := range []struct {
		name string
		a    *auth.Auth
		want string
	}{
		{"cn", cnAuth(), "5.9.3"},
		{"global", globalAuth(), "5.9.4"},
	} {
		c := New()
		req, _ := http.NewRequest(http.MethodPost, "https://chat.example/v2/chat/completions", nil)
		c.ChatHeaders(req, tc.a, "", ChatMeta{})
		gotUA := req.Header.Get("User-Agent")
		gotIDE := req.Header.Get("X-IDE-Version")

		if gotIDE != tc.want {
			t.Errorf("%s X-IDE-Version = %q want %q", tc.name, gotIDE, tc.want)
		}
		// UA 版本段（首段 `WorkBuddy/<ver>` 的 ver）必须与 X-IDE-Version 相同。
		first := strings.Fields(gotUA)[0]
		uaVer := strings.TrimPrefix(first, "WorkBuddy/")
		if uaVer != gotIDE {
			t.Errorf("%s UA 版本 %q != X-IDE-Version %q (UA=%q)", tc.name, uaVer, gotIDE, gotUA)
		}
	}
}

// TestUAParityClientVersionOverrideWins config upstream.client_version 覆盖时，
// UA 的两处版本段与 X-IDE-Version **同时**变成该值（不允许只改一处）。
func TestUAParityClientVersionOverrideWins(t *testing.T) {
	c := New()
	c.ClientVersion = "9.9.9"
	a := cnAuth()
	wantUA := "WorkBuddy/9.9.9 WorkBuddy/9.9.9 CLI/2.165.0"
	if got := c.defaultWorkBuddyUAFor(a); got != wantUA {
		t.Errorf("override UA = %q want %q", got, wantUA)
	}
	req, _ := http.NewRequest(http.MethodPost, "https://chat.example/v2/chat/completions", nil)
	c.ChatHeaders(req, a, "", ChatMeta{})
	if got := req.Header.Get("X-IDE-Version"); got != "9.9.9" {
		t.Errorf("override X-IDE-Version = %q want 9.9.9", got)
	}
	if got := req.Header.Get("User-Agent"); got != wantUA {
		t.Errorf("override chat UA = %q want %q", got, wantUA)
	}
}

// TestUAParityUserAgentOverrideIsVerbatim config upstream.user_agent 非空时 UA
// **整串**等于该值：不拼装、不追加品牌/CLI 段（既有覆盖语义保持不动）。
func TestUAParityUserAgentOverrideIsVerbatim(t *testing.T) {
	const custom = "MyGateway/1.2.3 (custom)"
	c := New()
	c.UserAgent = custom
	for _, a := range []*auth.Auth{cnAuth(), globalAuth()} {
		req, _ := http.NewRequest(http.MethodPost, "https://chat.example/v2/chat/completions", nil)
		c.ChatHeaders(req, a, "", ChatMeta{})
		if got := req.Header.Get("User-Agent"); got != custom {
			t.Errorf("UA = %q want %q（显式覆盖必须整串生效）", got, custom)
		}
	}
}

// TestUAParityBillingUASingleSegment billing/checkin 域 UA 仍是单段
// `WorkBuddy/<ver>`（官方 banner 显式覆写形态，不带 CLI 段），版本按 realm 取。
func TestUAParityBillingUASingleSegment(t *testing.T) {
	c := New()
	for _, tc := range []struct {
		name string
		a    *auth.Auth
		want string
	}{
		{"cn", cnAuth(), "WorkBuddy/5.9.3"},
		{"global", globalAuth(), "WorkBuddy/5.9.4"},
	} {
		got := c.billingUA(tc.a)
		if got != tc.want {
			t.Errorf("%s billingUA = %q want %q", tc.name, got, tc.want)
		}
		if n := len(strings.Fields(got)); n != 1 {
			t.Errorf("%s billingUA = %q fields=%d want 1", tc.name, got, n)
		}
	}
}
