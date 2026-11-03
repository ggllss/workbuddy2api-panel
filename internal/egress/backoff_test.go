// backoff_test.go 出口级退避的行为测试。
//
// 这套测试的存在理由：账号级熔断把连续失败记在**账号**头上（分钟级起、翻倍到
// 数小时），一次链路抖动会把池里每个账号轮流烧一遍（参考实现现场：4 小时 196 次
// URLError 陆续关掉 13 个账号）。出口级退避把这类失败改记在**出口**上，只换来
// **秒级**全局退避——所以这里断言的重点是「阶梯形状」和「过窗重置」，而不是
// 某个函数的返回值不为 nil。
package egress

import (
	"testing"
	"time"
)

// identityJitter 关掉随机性，让退避时长等于裸阶梯值。
func identityJitter(d time.Duration) time.Duration { return d }

// fixedClock 可推进的测试时钟。
type fixedClock struct{ t time.Time }

func (c *fixedClock) now() time.Time          { return c.t }
func (c *fixedClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func TestLadderGrowthAndCap(t *testing.T) {
	cases := []struct {
		streak int
		want   time.Duration
		why    string
	}{
		{0, Base, "streak<=0 视为首次抖动，从 Base 起"},
		{1, Base, "首次"},
		{2, 2 * Base, "翻倍"},
		{3, 4 * Base, "翻倍"},
		{4, 8 * Base, "翻倍"},
		{5, 15 * time.Second, "8s 再翻倍是 16s，超过默认封顶 15s → 封顶"},
		{6, 15 * time.Second, "封顶后保持"},
		{50, 15 * time.Second, "长连续不溢出、仍封顶"},
	}
	for _, c := range cases {
		if got := Ladder(c.streak, DefaultMax); got != c.want {
			t.Errorf("Ladder(%d)=%v want %v（%s）", c.streak, got, c.want, c.why)
		}
	}
	// 封顶可配：Max=4s 时阶梯到 4s 就停。
	if got := Ladder(10, 4*time.Second); got != 4*time.Second {
		t.Errorf("Ladder(10, 4s)=%v want 4s（封顶应由参数决定）", got)
	}
}

// TestNoteGrowsLadderAndWaitTracksDeadline 连续抖动：阶梯逐次增长，Wait 跟随到期时刻。
func TestNoteGrowsLadderAndWaitTracksDeadline(t *testing.T) {
	clk := &fixedClock{t: time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)}
	g := New(Config{Enabled: true, Max: DefaultMax})
	g.SetClock(clk.now)
	g.SetJitter(identityJitter)

	want := []time.Duration{Base, 2 * Base, 4 * Base, 8 * Base, DefaultMax, DefaultMax}
	for i, w := range want {
		if got := g.Wait(Direct); got != 0 {
			t.Fatalf("第 %d 次抖动前 Wait=%v，应为 0（此时还没进退避）", i+1, got)
		}
		if got := g.Note(Direct); got != w {
			t.Fatalf("第 %d 次 Note=%v want %v（阶梯）", i+1, got, w)
		}
		if got := g.Streak(Direct); got != i+1 {
			t.Fatalf("第 %d 次后 Streak=%d want %d", i+1, got, i+1)
		}
		// 到期前 Wait 应为剩余时长。
		if got := g.Wait(Direct); got != w {
			t.Fatalf("第 %d 次后 Wait=%v want %v", i+1, got, w)
		}
		clk.advance(w)
		if got := g.Wait(Direct); got != 0 {
			t.Fatalf("退避到期后 Wait=%v，应为 0（可立即发请求）", got)
		}
	}
}

// TestStreakResetsAfterWindow 距上次抖动超过 StreakReset：阶梯回到 Base，不再接着翻倍。
func TestStreakResetsAfterWindow(t *testing.T) {
	clk := &fixedClock{t: time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)}
	g := New(Config{Enabled: true, Max: DefaultMax})
	g.SetClock(clk.now)
	g.SetJitter(identityJitter)

	g.Note(Direct)
	g.Note(Direct) // streak=2 → 下次应为 4s
	if got := g.Streak(Direct); got != 2 {
		t.Fatalf("Streak=%d want 2", got)
	}

	clk.advance(StreakReset + time.Second)
	if got := g.Streak(Direct); got != 0 {
		t.Fatalf("过窗后 Streak=%d want 0", got)
	}
	if got := g.Note(Direct); got != Base {
		t.Fatalf("过窗后首次抖动=%v want Base(%v)——计数应已重置", got, Base)
	}
}

// TestNoteSuccessClears 成功即清零：出口又通了就不该背着上一次的阶梯。
func TestNoteSuccessClears(t *testing.T) {
	clk := &fixedClock{t: time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)}
	g := New(Config{Enabled: true, Max: DefaultMax})
	g.SetClock(clk.now)
	g.SetJitter(identityJitter)

	g.Note(Direct)
	g.Note(Direct)
	g.Note(Direct)
	if g.Wait(Direct) == 0 {
		t.Fatal("三次抖动后应处于退避中")
	}
	g.NoteSuccess(Direct)
	if got := g.Wait(Direct); got != 0 {
		t.Errorf("成功后 Wait=%v want 0（退避应被清掉）", got)
	}
	if got := g.Streak(Direct); got != 0 {
		t.Errorf("成功后 Streak=%d want 0", got)
	}
	if got := g.Note(Direct); got != Base {
		t.Errorf("成功后再抖动的阶梯=%v want Base（不该接着旧阶梯）", got)
	}
}

// TestKeysAreIndependent 出口级计数按出口键隔离：一个出口抖动不影响另一个出口。
func TestKeysAreIndependent(t *testing.T) {
	clk := &fixedClock{t: time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)}
	g := New(Config{Enabled: true, Max: DefaultMax})
	g.SetClock(clk.now)
	g.SetJitter(identityJitter)

	proxyA := KeyFor("http://proxy-a:8080")
	proxyB := KeyFor("socks5://proxy-b:1080")

	g.Note(proxyA)
	g.Note(proxyA)
	if g.Wait(Direct) != 0 {
		t.Error("proxyA 抖动导致直连出口被退避——出口键隔离失效")
	}
	if g.Wait(proxyB) != 0 {
		t.Error("proxyA 抖动导致 proxyB 被退避——出口键隔离失效")
	}
	if g.Wait(proxyA) == 0 {
		t.Error("proxyA 自身应在退避中")
	}
	// 两个出口的阶梯互不影响：B 第一次抖动仍是 Base。
	if got := g.Note(proxyB); got != Base {
		t.Errorf("proxyB 首次抖动=%v want Base（不应继承 proxyA 的阶梯）", got)
	}
}

// TestKeyForNormalization 空代理串回落直连；同一代理的不同写法共用一个键。
func TestKeyForNormalization(t *testing.T) {
	if got := KeyFor(""); got != Direct {
		t.Errorf("KeyFor(\"\")=%q want %q", got, Direct)
	}
	if got := KeyFor("   \t\n "); got != Direct {
		t.Errorf("KeyFor(空白)=%q want %q", got, Direct)
	}
	a := KeyFor("  HTTP://Proxy-A:8080 ")
	b := KeyFor("http://proxy-a:8080")
	if a != b {
		t.Errorf("同一代理不同写法应共用出口键：%q vs %q", a, b)
	}
	if a == Direct {
		t.Error("非空代理不应回落直连")
	}
}

// TestDisabledGateIsNoop 关掉开关即完全退化（handler 侧逐字保留改造前行为）。
func TestDisabledGateIsNoop(t *testing.T) {
	g := New(Config{Enabled: false})
	if g.Enabled() {
		t.Fatal("Enabled() 应为 false")
	}
	if got := g.Note(Direct); got != 0 {
		t.Errorf("关闭时 Note=%v want 0", got)
	}
	if got := g.Wait(Direct); got != 0 {
		t.Errorf("关闭时 Wait=%v want 0", got)
	}
	if got := g.Streak(Direct); got != 0 {
		t.Errorf("关闭时 Streak=%d want 0", got)
	}
}

// TestConfigureDisableClearsInFlight 关掉开关应清空在途退避，避免被上一轮抖动拖着等。
func TestConfigureDisableClearsInFlight(t *testing.T) {
	clk := &fixedClock{t: time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)}
	g := New(Config{Enabled: true, Max: DefaultMax})
	g.SetClock(clk.now)
	g.SetJitter(identityJitter)

	g.Note(Direct)
	g.Note(Direct)
	if g.Wait(Direct) == 0 {
		t.Fatal("应先进入退避")
	}
	g.Configure(false, DefaultMax)
	if got := g.Wait(Direct); got != 0 {
		t.Errorf("关闭后 Wait=%v want 0（在途退避应被清空）", got)
	}
	// 重新打开后从干净状态起步。
	g.Configure(true, DefaultMax)
	if got := g.Streak(Direct); got != 0 {
		t.Errorf("重新打开后 Streak=%d want 0", got)
	}
	if got := g.Note(Direct); got != Base {
		t.Errorf("重新打开后首次抖动=%v want Base", got)
	}
}

// TestConfigureMaxOverride 封顶可由配置热改。
func TestConfigureMaxOverride(t *testing.T) {
	clk := &fixedClock{t: time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)}
	g := New(Config{Enabled: true, Max: 4 * time.Second})
	g.SetClock(clk.now)
	g.SetJitter(identityJitter)

	want := []time.Duration{Base, 2 * Base, 4 * time.Second, 4 * time.Second}
	for i, w := range want {
		if got := g.Note(Direct); got != w {
			t.Fatalf("第 %d 次 Note=%v want %v（Max=4s 的封顶）", i+1, got, w)
		}
	}
	// Max<=0 回落默认封顶。
	g2 := New(Config{Enabled: true, Max: 0})
	g2.SetClock(clk.now)
	g2.SetJitter(identityJitter)
	for i := 0; i < 6; i++ {
		g2.Note(Direct)
	}
	if got := g2.Streak(Direct); got != 6 {
		t.Fatalf("Streak=%d want 6", got)
	}
}

// TestJitterWithinRange 默认随机抖动必须落在 0.75~1.25 倍之间，
// 否则要么退避过短（等于没退避）要么过长（用户侧感知成卡死）。
func TestJitterWithinRange(t *testing.T) {
	lo, hi := time.Duration(float64(Base)*JitterLow), time.Duration(float64(Base)*JitterHigh)
	for i := 0; i < 200; i++ {
		clk := &fixedClock{t: time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)}
		g := New(Config{Enabled: true, Max: DefaultMax})
		g.SetClock(clk.now) // 用真实随机抖动
		got := g.Note(Direct)
		if got < lo || got > hi {
			t.Fatalf("首次抖动=%v 超出 [%v, %v] 区间", got, lo, hi)
		}
	}
}

// TestNilGateSafe nil *Gate 是合法的「未启用」形态：所有方法退化为无操作且不 panic。
func TestNilGateSafe(t *testing.T) {
	var g *Gate
	if g.Enabled() {
		t.Error("nil Gate 的 Enabled 应为 false")
	}
	if got := g.Note(Direct); got != 0 {
		t.Errorf("nil Gate Note=%v want 0", got)
	}
	if got := g.Wait(Direct); got != 0 {
		t.Errorf("nil Gate Wait=%v want 0", got)
	}
	if got := g.Streak(Direct); got != 0 {
		t.Errorf("nil Gate Streak=%d want 0", got)
	}
	g.Configure(true, DefaultMax) // 不应 panic
	g.NoteSuccess(Direct)
	g.Reset()
	g.SetClock(nil)
	g.SetJitter(nil)
}
