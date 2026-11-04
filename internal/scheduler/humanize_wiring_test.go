// humanize_wiring_test.go 钉住「humanize.go 的抖动真的接进了调度器」这件事。
//
// 背景：抖动实现（humanize.go）早已写好但全仓无人调用——死代码，所以所有账号仍在
// 同一秒打上游（签到 09:00、活跃上报 10:00、旅行 09:00），是最显眼的机器特征。
// 本文件分两层钉：
//   - 单元层：窗口 / start / uid 与偏移的关系（关闭即零延迟、按账号稳定、恒不跨窗口）；
//   - 接线层：三个任务循环**确实**在「廉价过滤之后、第一次上游调用之前」查询了窗口
//     （窗口 >0 时入口被拖慢、窗口 0 时立刻跑完——这个差分只有真接上了才成立）。
package scheduler

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// jitterTestUIDs 固定候选账号列表。散列是纯函数，所以「哪个 uid 拿到哪个偏移」在
// 同一 (任务, 日, 窗口) 下永远一致——下面的断言全部可复现，不依赖随机。
func jitterTestUIDs(n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf("jit-%03d", i))
	}
	return out
}

// jitterKey 复刻 accountJitterDelay 内部的散列 key。
//
// 为什么测试要自己拼这个 key：accountJitterDelay 返回的是「还差多少」（随时间递减），
// 不是「偏移」本身；「同一账号同一天偏移恒定」这条不变量只存在于 stableOffset 层，
// 所以稳定性/上限必须在 (key, window) 这一层断言，再单独验证 accountJitterDelay 与
// 它的关系。
func jitterKey(kind taskKind, start time.Time, uid string) string {
	return kindKey(kind) + "|" + travelDay(start) + "|" + uid
}

// pinJitter 测试结束还原窗口：jitterWindowNanos 是包级 atomic，泄漏会污染同包其他
// 时序测试（它们默认窗口 0 才是安全的）。
func pinJitter(t *testing.T) {
	t.Helper()
	old := jitterWindowNanos.Load()
	t.Cleanup(func() { jitterWindowNanos.Store(old) })
}

// jitterUIDWithOffset 在固定候选里找一个在该 (任务, start, 窗口) 下偏移恰为 want 的
// 账号。散列纯函数 ⇒ 这是一次性确定的选择而不是撞运气；找不到说明散列/取模口径变了。
func jitterUIDWithOffset(t *testing.T, kind taskKind, start time.Time, window, want time.Duration) string {
	t.Helper()
	for _, uid := range jitterTestUIDs(64) {
		if stableOffset(jitterKey(kind, start, uid), window) == want {
			return uid
		}
	}
	t.Fatalf("64 个候选 uid 中无偏移 %v 者（kind=%s window=%v）", want, kindKey(kind), window)
	return ""
}

// TestJitterWindowZeroMeansNoDelay 窗口 0 = 逐字旧行为：任何账号、任何任务零延迟。
// 这是「关掉抖动就完全恢复改造前行为」的唯一保证，也是既有排程时序测试的护栏。
func TestJitterWindowZeroMeansNoDelay(t *testing.T) {
	pinJitter(t)
	s := newTestScheduler()
	s.SetJitterWindow(0)

	start := time.Now()
	for _, kind := range []taskKind{taskCheckin, taskTravel, taskActivity} {
		for _, uid := range jitterTestUIDs(10) {
			if d := s.accountJitterDelay(start, kind, uid); d != 0 {
				t.Fatalf("window=0 kind=%s uid=%s delay=%v want 0", kindKey(kind), uid, d)
			}
		}
	}
}

// TestJitterZeroStartMeansManualNoDelay start 为零值 = 手动执行（面板按钮 / Run*Now）：
// 即使窗口很大也恒零延迟。手动入口与排程入口的唯一区别就是这个零值 start。
func TestJitterZeroStartMeansManualNoDelay(t *testing.T) {
	pinJitter(t)
	s := newTestScheduler()
	s.SetJitterWindow(120 * time.Second)

	for _, kind := range []taskKind{taskCheckin, taskTravel, taskActivity} {
		for _, uid := range jitterTestUIDs(10) {
			if d := s.accountJitterDelay(time.Time{}, kind, uid); d != 0 {
				t.Fatalf("start=零值 kind=%s uid=%s delay=%v want 0（手动执行不得抖动）", kindKey(kind), uid, d)
			}
		}
	}
}

// TestJitterOffsetStablePerUID 同一 (任务, 日, uid) 的偏移恒定（多次求值完全相等），
// 且不同 uid 的偏移被摊开：10 个账号里至少 8 个不同（120s 窗口实测 10/10）。
func TestJitterOffsetStablePerUID(t *testing.T) {
	pinJitter(t)
	s := newTestScheduler()
	s.SetJitterWindow(120 * time.Second)

	const window = 120 * time.Second
	start := time.Now()
	uids := jitterTestUIDs(10)

	seen := make(map[time.Duration]struct{}, len(uids))
	for _, uid := range uids {
		off := stableOffset(jitterKey(taskCheckin, start, uid), window)
		for i := 0; i < 50; i++ {
			if got := stableOffset(jitterKey(taskCheckin, start, uid), window); got != off {
				t.Fatalf("uid=%s 第 %d 次求值偏移=%v want %v（同账号同一天必须恒定）", uid, i, got, off)
			}
		}
		seen[off] = struct{}{}

		// accountJitterDelay = 偏移 - 已过时间：既不得超过偏移，也不该无故缩水。
		if d := s.accountJitterDelay(start, taskCheckin, uid); d > off || d < off-100*time.Millisecond {
			t.Fatalf("uid=%s accountJitterDelay=%v 与偏移 %v 不一致（应约等于 偏移-已过时间）", uid, d, off)
		}
	}
	if len(seen) < 8 {
		t.Fatalf("10 个账号只摊出 %d 个不同偏移（want >=8）：%v", len(seen), seen)
	}
}

// TestJitterOffsetNeverCrossesWindow 偏移恒 < 窗口（多个窗口验证）：抖动只能让账号
// **晚一点**开始，绝不能落到下一个周期之后（那会漏跑或重复跑）。
func TestJitterOffsetNeverCrossesWindow(t *testing.T) {
	pinJitter(t)
	s := newTestScheduler()

	start := time.Now()
	for _, window := range []time.Duration{120 * time.Second, 60 * time.Second, 900 * time.Second} {
		s.SetJitterWindow(window)
		for _, kind := range []taskKind{taskCheckin, taskTravel, taskActivity} {
			for _, uid := range jitterTestUIDs(10) {
				off := stableOffset(jitterKey(kind, start, uid), window)
				if off < 0 || off >= window {
					t.Fatalf("window=%v kind=%s uid=%s 偏移=%v 越界（须 0 <= 偏移 < 窗口）", window, kindKey(kind), uid, off)
				}
				if d := s.accountJitterDelay(start, kind, uid); d >= window {
					t.Fatalf("window=%v kind=%s uid=%s 延迟=%v 越界", window, kindKey(kind), uid, d)
				}
			}
		}
	}
}

// TestJitterSubSecondWindowDegradesToZero 窗口 < 2s 时偏移恒为 0。
//
// 这是 humanize.go 的既存口径，不是本次接线引入的：stableOffset 按**整秒**取模，
// window/1s 向下取整后 <=1，取模基数就是 1，h%1 恒 0（500ms 与 1500ms 都一样）。
// 任务书期望「500ms 仍能产生非零偏移（实现里按 1 秒处理）」，但按 1 秒处理的实际结果
// 就是「取模基数 1 ⇒ 偏移 0」；本次接线不改 humanize.go 的实现逻辑，故这里钉住现状：
// 窗口值本身被完整保存（不是被钳成 0），只是不产生抖动。
// 若要让 sub-2s 生效，须把 stableOffset 的粒度改成纳秒（改 humanize.go）。
func TestJitterSubSecondWindowDegradesToZero(t *testing.T) {
	pinJitter(t)
	s := newTestScheduler()

	start := time.Now()
	for _, window := range []time.Duration{500 * time.Millisecond, 1500 * time.Millisecond} {
		s.SetJitterWindow(window)
		if got := s.jitterWindow(); got != window {
			t.Fatalf("jitterWindow()=%v want %v（窗口值必须原样保存，不被钳 0）", got, window)
		}
		for _, uid := range jitterTestUIDs(10) {
			if d := s.accountJitterDelay(start, taskCheckin, uid); d != 0 {
				t.Fatalf("window=%v uid=%s delay=%v want 0（整秒取模下 sub-2s 窗口不产生抖动）", window, uid, d)
			}
		}
	}
}

// TestJitterWindowHotSwap 热改生效：面板保存配置后窗口立即是新值，改回 0 立即关闭。
func TestJitterWindowHotSwap(t *testing.T) {
	pinJitter(t)
	s := newTestScheduler()

	s.SetJitterWindow(120 * time.Second)
	if got := s.jitterWindow(); got != 120*time.Second {
		t.Fatalf("jitterWindow()=%v want 2m", got)
	}
	s.SetJitterWindow(0)
	if got := s.jitterWindow(); got != 0 {
		t.Fatalf("jitterWindow()=%v want 0（关闭）", got)
	}
	s.SetJitterWindow(-5 * time.Second) // 负值 = 关闭，不 panic 不取反
	if got := s.jitterWindow(); got != 0 {
		t.Fatalf("jitterWindow()=%v want 0（负值按关闭处理）", got)
	}
}

// ---- 接线层：三个任务循环真的查询了抖动窗口 ----

// TestRunCheckinWaitsForJitterWindow 签到排程入口在第一次上游调用前按窗口等待；
// 同一入口窗口置 0 后立刻跑完——差分证明那 1 秒来自抖动而不是别处。
func TestRunCheckinWaitsForJitterWindow(t *testing.T) {
	pinJitter(t)
	f := &fakeUpstream{resourceRemain: 500}
	srv := f.server()
	defer srv.Close()

	start := time.Now()
	uid := jitterUIDWithOffset(t, taskCheckin, start, 2*time.Second, time.Second)
	p := pool.New("")
	p.Add(&auth.Auth{UID: uid, AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	s := New(Config{Pool: p, Upstream: &upstream.Client{
		HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL,
	}})

	s.SetJitterWindow(2 * time.Second)
	begin := time.Now()
	s.runCheckin(context.Background(), start)
	if elapsed := time.Since(begin); elapsed < 900*time.Millisecond {
		t.Fatalf("窗口 2s / 偏移 1s 却只用了 %v：runCheckin 没在签到前查询抖动窗口", elapsed)
	}
	if n := f.checkinCalls.Load(); n != 1 {
		t.Fatalf("checkin calls=%d want 1（抖完照常签到一次）", n)
	}

	s.SetJitterWindow(0)
	begin = time.Now()
	s.runCheckin(context.Background(), start)
	if elapsed := time.Since(begin); elapsed > 300*time.Millisecond {
		t.Fatalf("窗口 0 仍耗时 %v：关闭后必须立刻执行", elapsed)
	}
	if n := f.checkinCalls.Load(); n != 2 {
		t.Fatalf("checkin calls=%d want 2", n)
	}
}

// TestRunActivityWaitsForJitterWindow 活跃上报排程入口同上。
func TestRunActivityWaitsForJitterWindow(t *testing.T) {
	pinJitter(t)
	fastActivity(t) // 账号间限速置 0：这里只量抖动，不量限速
	stub := &reportStub{}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	start := time.Now()
	uid := jitterUIDWithOffset(t, taskActivity, start, 2*time.Second, time.Second)
	p := pool.New("")
	p.Add(&auth.Auth{UID: uid, AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	s := New(Config{Pool: p, Upstream: &upstream.Client{
		HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL,
	}})

	s.SetJitterWindow(2 * time.Second)
	begin := time.Now()
	s.runActivity(context.Background(), start)
	if elapsed := time.Since(begin); elapsed < 900*time.Millisecond {
		t.Fatalf("窗口 2s / 偏移 1s 却只用了 %v：runActivity 没在上报前查询抖动窗口", elapsed)
	}
	if n := stub.calls.Load(); n != 1 {
		t.Fatalf("report calls=%d want 1", n)
	}

	s.SetJitterWindow(0)
	begin = time.Now()
	s.runActivity(context.Background(), start)
	if elapsed := time.Since(begin); elapsed > 300*time.Millisecond {
		t.Fatalf("窗口 0 仍耗时 %v：关闭后必须立刻执行", elapsed)
	}
	if n := stub.calls.Load(); n != 2 {
		t.Fatalf("report calls=%d want 2", n)
	}
}

// TestRunTravelWaitsForJitterWindow 旅行排程入口同上（且账号间限速 first 逻辑未被打乱：
// 单账号不睡 travelAccountDelay）。
func TestRunTravelWaitsForJitterWindow(t *testing.T) {
	pinJitter(t)
	f := &fakeUpstream{}
	srv := f.server()
	defer srv.Close()

	start := time.Now()
	uid := jitterUIDWithOffset(t, taskTravel, start, 2*time.Second, time.Second)
	p := pool.New("")
	p.Add(&auth.Auth{UID: uid, AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	s := New(Config{Pool: p, Upstream: &upstream.Client{
		HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL,
	}})

	s.SetJitterWindow(2 * time.Second)
	begin := time.Now()
	s.runTravel(context.Background(), start)
	if elapsed := time.Since(begin); elapsed < 900*time.Millisecond {
		t.Fatalf("窗口 2s / 偏移 1s 却只用了 %v：runTravel 没在旅行前查询抖动窗口", elapsed)
	}
	if n := f.travelCalls.Load(); n != 1 {
		t.Fatalf("travel status calls=%d want 1", n)
	}

	s.SetJitterWindow(0)
	begin = time.Now()
	s.runTravel(context.Background(), start)
	if elapsed := time.Since(begin); elapsed > 300*time.Millisecond {
		t.Fatalf("窗口 0 仍耗时 %v：关闭后必须立刻执行", elapsed)
	}
	if n := f.travelCalls.Load(); n != 2 {
		t.Fatalf("travel status calls=%d want 2", n)
	}
}

// TestRunNowEntrypointsIgnoreJitter 面板手动入口（Run*Now）在窗口打开时行为不变：
// 它们传零值 start，即使窗口 2s 也不等待。
func TestRunNowEntrypointsIgnoreJitter(t *testing.T) {
	pinJitter(t)
	fastActivity(t)
	f := &fakeUpstream{resourceRemain: 500}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "jit-000", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	s := New(Config{Pool: p, Upstream: &upstream.Client{
		HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL,
	}})
	s.SetJitterWindow(120 * time.Second)

	begin := time.Now()
	s.RunCheckinNow()
	s.RunActivityNow()
	s.RunTravelNow()
	if elapsed := time.Since(begin); elapsed > 500*time.Millisecond {
		t.Fatalf("手动入口耗时 %v：Run*Now 传零值 start，必须零抖动", elapsed)
	}
}
