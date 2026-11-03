// humanize.go 定时任务的人味化抖动：账号任务不再「同一秒一起打上游」。
//
// 背景（本仓风控复盘，与参考实现 ref-misaka/internal/scheduler/humanize.go 同源）：
// 12 个账号在同一出口 IP 上以「签到 09:00:00、余额刷新每 5 分钟整点、保活 22:00:00」
// 的完全一致节拍活动，是最容易识别的机器形状——人不会在同一秒对同一服务做同一件事。
//
// 设计口径（三条，都刻意与「每次随机」相反）：
//  1. **按账号稳定**：偏移由 (任务, 上游自然日, uid) 散列派生，同一账号同一天永远
//     同一偏移。每次随机会让任务不可复现（测试无法断言、排障无法解释），而且「间隔
//     恒定但相位随机抖动」与「相位稳定但间隔被摊开」对上游呈现的机器特征没区别。
//  2. **不跨周期边界**：偏移恒 < 抖动窗口，窗口本身远小于最小任务周期（签到 12h、
//     保活/夜猫子 24h）。因此抖动只可能让某个账号**晚一点**开始，绝不可能落到下一个
//     周期之后——那会造成漏跑或重复跑。余额刷新周期最短（默认 5 分钟），故那里额外
//     把窗口压到 interval/4（见 balanceJitterWindow）。
//  3. **幅度 0 = 逐字旧行为**：window<=0 时所有函数返回 0，不 sleep 一个纳秒。
//
// 偏移以「批次起点 + 偏移」的形式生效（而不是「每账号固定再睡多久」）：串行遍历时
// 前面账号的耗时已经消耗掉一部分窗口，按「还差多少」算就不会把批次总时长撑成
// 账号数 × 窗口（那会把 12 个号的签到拖到十几分钟）。
package scheduler

import (
	"context"
	"hash/fnv"
	"sync/atomic"
	"time"
)

// defaultJitterSeconds schedule.jitter_seconds 的缺省值。
//
// 依据：12 个账号摊在 120 秒窗口内平均 10 秒一个，已完全打散「同一秒」的形状；而
// 120s 相对最小任务周期（余额刷新 5 分钟 = 300s）留出 2.5 倍余量，配合 balanceJitterWindow
// 的 interval/4 封顶后不可能跨周期。再大（如 600s）会吃掉半个余额刷新周期，收益不增。
const defaultJitterSeconds = 120

// jitterWindowNanos 抖动窗口（纳秒，0 = 关闭）。atomic 读写：面板保存配置时可热改。
var jitterWindowNanos atomic.Int64

// SetJitterWindow 热改抖动窗口；<=0 关闭抖动（逐字恢复改造前行为）。
func (s *Scheduler) SetJitterWindow(d time.Duration) {
	if d < 0 {
		d = 0
	}
	jitterWindowNanos.Store(int64(d))
}

// jitterWindow 当前抖动窗口。
func (s *Scheduler) jitterWindow() time.Duration {
	return time.Duration(jitterWindowNanos.Load())
}

// kindKey 任务类型在散列里的稳定标识。不能用 iota 的数值：常量顺序调整会让所有
// 账号的偏移一起漂移（那正是「同一天同一账号偏移一致」要避免的不可复现）。
func kindKey(k taskKind) string {
	switch k {
	case taskCheckin:
		return "checkin"
	case taskTravel:
		return "travel"
	case taskActivity:
		return "activity"
	case taskKeepalive:
		return "keepalive"
	case taskBlackcat:
		return "blackcat"
	case taskGrowth:
		return "growth"
	case taskBalance:
		return "balance"
	default:
		return "task"
	}
}

// stableOffset 由 key 稳定派生的 [0, window) 偏移。
//
// 用 FNV-1a 而非 math/rand：散列是纯函数，同 key 恒同值，无需持有种子也无需可注入
// 的随机源——「稳定」这件事由算法本身保证，而不是靠调用方记得别重算。
// 粒度取整秒：要打散的是「同一秒」，毫秒级精度没有收益，还会让日志难读。
func stableOffset(key string, window time.Duration) time.Duration {
	if window <= 0 {
		return 0
	}
	secs := int64(window / time.Second)
	if secs <= 0 {
		// 窗口不足 1 秒：按 1 秒处理，否则取模会退化成恒 0（抖动静默失效）。
		secs = 1
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	return time.Duration(h.Sum64()%uint64(secs)) * time.Second
}

// accountJitterDelay 返回该账号在 batchStart 起算的抖动**还差多少**：已过点返回 0。
//
// start 为零值表示「手动立即执行」（面板按钮 / Run*Now / 测试）：恒返回 0，零延迟。
// 这是手动入口与排程入口唯一的区别，不需要额外的布尔参数到处传。
func (s *Scheduler) accountJitterDelay(start time.Time, kind taskKind, uid string) time.Duration {
	if start.IsZero() {
		return 0
	}
	w := s.jitterWindow()
	if w <= 0 || uid == "" {
		return 0
	}
	// 自然日按上游 CST 口径（travelDay），不是宿主时区：UTC 容器里跨零点会让
	// 同一账号在 CST 一天内换两次偏移，等于「每次随机」。
	key := kindKey(kind) + "|" + travelDay(start) + "|" + uid
	if d := time.Until(start.Add(stableOffset(key, w))); d > 0 {
		return d
	}
	return 0
}

// sleepJitter 该账号任务前的抖动等待；ctx 取消立即返回 false（优雅停机不等抖完）。
func (s *Scheduler) sleepJitter(ctx context.Context, start time.Time, kind taskKind, uid string) bool {
	return sleepCtx(ctx, s.accountJitterDelay(start, kind, uid))
}

// balanceJitterWindow 余额刷新的抖动窗口：不超过刷新间隔的 1/4。
//
// 为什么单独压：余额刷新是唯一「分钟级」周期（默认 5 分钟），窗口若接近间隔，
// 一个账号这一轮的偏移就会推到下一轮开始之后——两轮重叠、同一账号连查两次余额
// （重复跑），同时后一轮的账号又被前一轮占着名额。1/4 保证上一轮必然在本轮
// 开始前结束。
func balanceJitterWindow(interval time.Duration) time.Duration {
	w := time.Duration(jitterWindowNanos.Load())
	if w <= 0 || interval <= 0 {
		return 0
	}
	if q := interval / 4; w > q {
		return q
	}
	return w
}
