// Package egress 出口级瞬时失败退避：把「链路/出口抖动」从账号熔断里摘出来。
//
// 背景（本仓 2026 事故 + 参考实现 ref-hub/wb_egress.py 的现场数据）：账号级熔断
// 把「连续 N 次硬失败」记在**账号**头上（30 分钟起、翻倍到 6 小时）。当失败来自
// 出口/链路的瞬时抖动而非账号本身时，这个口径会把一次抖动记到每个账号头上——
// 参考实现现场实测 4 小时内 196 次 URLError 陆续关掉 13 个账号，期间池子几乎空掉；
// 本仓也出现过短时间多号连续冷却、健康号从 5 掉到 2 的同形现象。
//
// 本包把这类失败改记在**出口**上：同一出口的账号共用一个计数器，于是一次抖动
// 只换来一段**秒级**的全局退避，而不是 N 个分钟级熔断。尺度是重点——账号级阶梯
// 是分钟级（30 分钟起），出口级阶梯是秒级（1 秒起、封顶 15 秒、带抖动）。
//
// 归责分流（哪些错误记出口、哪些仍记账号）由 upstream.FaultOfKind 判定，本包只
// 负责「记在哪个键上、退避多久」。两层分离是本仓既有风格：错误分类在 upstream，
// 罚号在 server/pool。
package egress

import (
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Key 出口身份。同一物理出口的账号落在同一个键上，于是共用一个连续计数器。
//
// 当前部署只有直连一个出口（不做代理池），所以恒为 Direct；抽象成键是为将来加
// 代理时自然扩展——那时同一代理串下的账号自动共用一个计数器，与参考实现的
// egress_key()（与连接池键同一口径）同一思路。
type Key string

// Direct 直连出口的键。所有不走代理的账号共享它。
const Direct Key = "direct"

// KeyFor 由代理串派生出口键：空串（或纯空白）= 直连；非空按规范化后的代理串
// 区分（去空白、转小写），将来加代理池时同一代理下的账号自动共用一个计数器。
func KeyFor(proxy string) Key {
	p := strings.ToLower(strings.TrimSpace(proxy))
	if p == "" {
		return Direct
	}
	return Key(p)
}

const (
	// Base 首次抖动的退避时长；之后每次翻倍。
	Base = 1 * time.Second
	// DefaultMax 退避封顶的默认值（配置 upstream.egress_backoff_max_seconds 可改）。
	DefaultMax = 15 * time.Second
	// StreakReset 距上次抖动超过这么久，就把连续计数当作新的一轮（而不是接着
	// 上一次翻倍）——与参考实现 EGRESS_STREAK_RESET 同口径。
	StreakReset = 60 * time.Second
	// JitterLow/JitterHigh 抖动的比例区间：实际退避在计算值的 0.75~1.25 倍之间，
	// 避免所有请求在退避到期后同步重试（齐步重试本身又是可识别的机器特征）。
	JitterLow  = 0.75
	JitterHigh = 1.25
)

// Config 运行期可热改的退避参数。
type Config struct {
	// Enabled 关闭时 Gate 全部方法退化为无操作（handler 侧逐字保留改造前行为）。
	Enabled bool
	// Max 退避封顶；<=0 回落 DefaultMax。
	Max time.Duration
}

// entry 单出口的连续抖动状态。
type entry struct {
	streak int
	last   time.Time // 上次抖动时刻（用于 StreakReset 判定）
	until  time.Time // 当前退避到期时刻
}

// Gate 出口级退避状态机。零值不可用（用 New）；nil *Gate 是合法的「未启用」形态，
// 所有方法 nil-safe 退化为无操作，方便 handler 在测试与裸用场景省掉判空。
type Gate struct {
	mu    sync.Mutex
	state map[Key]*entry
	cfg   atomic.Pointer[Config]

	// now 时钟（测试注入）；nil 时用 time.Now。
	now func() time.Time
	// jitter 退避抖动（测试注入确定值）；nil 时用 JitterLow~JitterHigh 均匀随机。
	jitter func(time.Duration) time.Duration
}

// New 构建。
func New(cfg Config) *Gate {
	g := &Gate{state: make(map[Key]*entry)}
	g.cfg.Store(&cfg)
	return g
}

// Configure 热改参数（面板保存配置时调用）：关闭立即清空在途退避，避免关掉开关
// 后仍被上一轮抖动拖着等。
func (g *Gate) Configure(enabled bool, max time.Duration) {
	if g == nil {
		return
	}
	if max <= 0 {
		max = DefaultMax
	}
	g.cfg.Store(&Config{Enabled: enabled, Max: max})
	if !enabled {
		g.mu.Lock()
		g.state = make(map[Key]*entry)
		g.mu.Unlock()
	}
}

// Enabled 报告退避是否启用（nil Gate = 未启用）。
func (g *Gate) Enabled() bool {
	if g == nil {
		return false
	}
	c := g.cfg.Load()
	return c != nil && c.Enabled
}

// maxWait 当前封顶。
func (g *Gate) maxWait() time.Duration {
	c := g.cfg.Load()
	if c == nil || c.Max <= 0 {
		return DefaultMax
	}
	return c.Max
}

func (g *Gate) clock() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now()
}

// Ladder 连续 streak 次抖动后、这一段出口的退避时长（未加抖动）。
// streak<=0 视为 1 次（首次抖动 Base 起）。
func Ladder(streak int, max time.Duration) time.Duration {
	if max <= 0 {
		max = DefaultMax
	}
	if streak < 1 {
		streak = 1
	}
	d := Base
	for k := 1; k < streak && d < max; k++ {
		d *= 2
		if d <= 0 { // 翻倍溢出成非正数：直接按封顶处理
			return max
		}
	}
	if d > max {
		d = max
	}
	return d
}

// jittered 给退避加抖动。d<=0 原样返回（零等待不抖动）。
func (g *Gate) jittered(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	if g.jitter != nil {
		return g.jitter(d)
	}
	f := JitterLow + rand.Float64()*(JitterHigh-JitterLow)
	return time.Duration(float64(d) * f)
}

// Note 记一次出口级瞬时失败，返回这一段出口本次应当退避的时长（带抖动）。
//
// 计数器在出口上、不是账号上，所以同一出口的多个账号一起抖时只会让这一段退避
// 变长，而不是各自进账号熔断。距上次抖动超过 StreakReset 时按新一轮重算。
func (g *Gate) Note(key Key) time.Duration {
	if !g.Enabled() {
		return 0
	}
	now := g.clock()
	g.mu.Lock()
	e := g.state[key]
	if e == nil || now.Sub(e.last) > StreakReset {
		e = &entry{}
		g.state[key] = e
	}
	e.streak++
	e.last = now
	wait := g.jittered(Ladder(e.streak, g.maxWait()))
	e.until = now.Add(wait)
	g.mu.Unlock()
	return wait
}

// NoteSuccess 这一段出口成功了一次：清零它的连续计数与退避。
//
// 与账号级 NoteSuccess 同一条规矩——成功即清零。抖动是瞬时的，一旦这一段出口
// 又通了，就不该再背着上一次的阶梯。
func (g *Gate) NoteSuccess(key Key) {
	if g == nil {
		return
	}
	g.mu.Lock()
	delete(g.state, key)
	g.mu.Unlock()
}

// Wait 这一段出口当前还该等多久（0 = 可以立即发请求）。
func (g *Gate) Wait(key Key) time.Duration {
	if !g.Enabled() {
		return 0
	}
	now := g.clock()
	g.mu.Lock()
	defer g.mu.Unlock()
	e := g.state[key]
	if e == nil {
		return 0
	}
	if d := e.until.Sub(now); d > 0 {
		return d
	}
	return 0
}

// Streak 这一段出口当前的连续抖动次数（排障与测试用；过窗视为 0）。
func (g *Gate) Streak(key Key) int {
	if g == nil {
		return 0
	}
	now := g.clock()
	g.mu.Lock()
	defer g.mu.Unlock()
	e := g.state[key]
	if e == nil || now.Sub(e.last) > StreakReset {
		return 0
	}
	return e.streak
}

// Reset 清空全部状态（测试用）。
func (g *Gate) Reset() {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.state = make(map[Key]*entry)
	g.mu.Unlock()
}

// SetClock 注入时钟（测试专用；nil 恢复 time.Now）。
func (g *Gate) SetClock(now func() time.Time) {
	if g == nil {
		return
	}
	g.now = now
}

// SetJitter 注入抖动函数（测试专用；nil 恢复随机抖动）。
func (g *Gate) SetJitter(fn func(time.Duration) time.Duration) {
	if g == nil {
		return
	}
	g.jitter = fn
}
