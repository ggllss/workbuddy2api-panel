// cost.go 把**当天**的 token 用量折成人民币金额（DeepSeek V4.1 Flash 官方价）。
//
// 为什么放在 usage 包而不是 cmd/server：
//   - 计费要按 scope 前缀筛桶、逐小时桶判峰谷，必须读未导出的 bucket 结构与
//     Recorder.buckets；放到外部包就意味着为了"读一下用量"把内部表示、锁与
//     分片口径暴露成公开 API，后续改桶结构会被外部计算代码绑死；
//   - 纯函数（Pricing / IsPeakHour）与聚合出口（CostToday）同处一包，测试可以
//     直接用 Recorder.Add 造假数据，不需要为了测试再造一层假接口。
//
// 计价口径（用户拍板，勿改）：
//   - 三档：缓存命中输入 / 缓存未命中输入 / 输出；
//   - 范围：当天**全部模型**统一折算，不做模型过滤；
//   - 逐小时桶判峰谷后套对应三档单价再累加（不能用单一价格算全天——否则夜间
//     流量会按高峰价翻倍虚高）；
//   - 峰谷定义见 IsPeakHour：北京时间工作日 [09:00,12:00) ∪ [14:00,18:00) 为
//     高峰，其余时段（含周末全天、extra_offpeak_dates 列出的节假日全天）为空闲。
//
// 本文件**只读内存**：不触发任何上游请求，也不改动任何桶（纯快照 + 遍历）。
package usage

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// UnitPerMillionTokens 响应里 pricing.unit 的固定文案：价格单位是
// 「元 / 百万 tokens」。写成常量而不是让调用方传，避免同一语义出现多种拼写，
// 让解析方可以硬编码比对。
const UnitPerMillionTokens = "CNY per 1M tokens"

// pricingUnit 金额单位（人民币）。上游 WorkBuddy 也用积分/元计价，本功能只用元。
const pricingCurrency = "CNY"

// beijingOffsetSeconds 北京时间固定偏移。所有峰谷判定都以北京时间为准
// （官方调价页写的时段就是北京时间），与进程本地时区无关。
const beijingOffsetSeconds = 8 * 3600

// beijingZone 北京时间时区。优先用系统 tzdata（镜像装了 tzdata，见 Dockerfile），
// 拿不到时退回固定 +08:00 时区——中国自 1991 年后无夏令时，固定偏移与
// Asia/Shanghai 在任意现实日期上等价，因此这条回落不会算错时段。
var beijingZone = func() *time.Location {
	if loc, err := time.LoadLocation("Asia/Shanghai"); err == nil {
		return loc
	}
	return time.FixedZone("Asia/Shanghai", beijingOffsetSeconds)
}()

// Pricing 三档单价（元 / 百万 tokens）+ 高峰/空闲两套。
//
// 字段名与 GET /api/cost/today 响应里的 pricing 对象逐一对应（外部程序要解析，
// 不要随意改名）。JSON 里不出现 extra_offpeak_dates：那个是"节假日日历"输入，
// 不是价格，故用 json:"-" 只作计算参数随行。
type Pricing struct {
	PeakInputCacheHit     float64 `json:"peak_input_cache_hit"`
	PeakInputCacheMiss    float64 `json:"peak_input_cache_miss"`
	PeakOutput            float64 `json:"peak_output"`
	OffPeakInputCacheHit  float64 `json:"offpeak_input_cache_hit"`
	OffPeakInputCacheMiss float64 `json:"offpeak_input_cache_miss"`
	OffPeakOutput         float64 `json:"offpeak_output"`
	// Unit 价格单位文案，恒为 "CNY per 1M tokens"（normalize 补齐）。
	Unit string `json:"unit"`

	// ExtraOffPeakDates 额外按**空闲**计价的自然日（"YYYY-MM-DD"，北京时间的
	// 日历日），用于法定节假日/调休。命中该日期的所有小时桶一律按空闲价，
	// 不再看星期与时刻。
	//
	// 为什么不做内置节假日表：中国法定节假日与调休每年由国务院公告，内置表
	// 必然在次年失效并**静默给出错价**（把节假日按高峰收），比不内置更糟。
	// 交给用户显式维护，缺省空 = 只按周末分峰谷。
	ExtraOffPeakDates []string `json:"-"`
}

// DefaultPricing 官方价（https://api-docs.deepseek.com/zh-cn/quick_start/pricing/，
// 模型列 deepseek-flash，版本 DeepSeek-V4.1-Flash-1017）。
//
// 空闲价 = 高峰价的一半（官方脚注），六个数字仍逐个写全而不做除法：官方调价时
// 「一半」这个关系未必成立，写死实际值才不会在调价后自动算出错价。
func DefaultPricing() Pricing {
	return Pricing{
		PeakInputCacheHit:     0.04,
		PeakInputCacheMiss:    2,
		PeakOutput:            8,
		OffPeakInputCacheHit:  0.02,
		OffPeakInputCacheMiss: 1,
		OffPeakOutput:         4,
		Unit:                  UnitPerMillionTokens,
	}
}

// normalize 补齐单位文案（Unit 是恒定值，不开放给用户配）。
func (p Pricing) normalize() Pricing {
	if p.Unit == "" {
		p.Unit = UnitPerMillionTokens
	}
	return p
}

// extraOffPeakSet 把日期列表转成集合；空项与空白忽略（面板逗号分隔易留空项）。
func (p Pricing) extraOffPeakSet() map[string]bool {
	if len(p.ExtraOffPeakDates) == 0 {
		return nil
	}
	set := make(map[string]bool, len(p.ExtraOffPeakDates))
	for _, d := range p.ExtraOffPeakDates {
		if d = strings.TrimSpace(d); d != "" {
			set[d] = true
		}
	}
	return set
}

// PricingHolder 价格配置的并发安全持有者（不可变快照 + 原子指针，同 livecfg.Holder
// 的思路）。面板在线改价后 Store 整体替换，下一个请求即用新价，无需重启。
//
// 为什么不塞进 livecfg.Snapshot：那会让 livecfg 反向依赖 usage 包（快照字段要写
// Pricing 类型），而 livecfg 的设计定位是"与业务包无关的通用热改快照"。这里的
// 存取点只有"算钱"一处，自带一个 holder 更内聚。
type PricingHolder struct {
	p atomic.Pointer[Pricing]
}

// NewPricingHolder 以初始价格构建。
func NewPricingHolder(p Pricing) *PricingHolder {
	h := &PricingHolder{}
	h.Store(p)
	return h
}

// Load 返回当前价格快照；Holder 为 nil 或从未 Store 时返回官方默认价
// （调用方无需判空——裸用/测试路径不会算出 0 元）。
func (h *PricingHolder) Load() Pricing {
	if h == nil {
		return DefaultPricing()
	}
	if p := h.p.Load(); p != nil {
		return p.normalize()
	}
	return DefaultPricing()
}

// Store 整体替换价格快照。
func (h *PricingHolder) Store(p Pricing) { h.p.Store(&p) }

// CostTokens 当天聚合出的 token 明细（一档一行，字段名即响应字段名）。
type CostTokens struct {
	// Prompt 上游报告的 prompt tokens 合计（= 命中 + 未命中，含历史上没有
	// 缓存维度观测的部分）。
	Prompt int64 `json:"prompt"`
	// CacheHit 前缀缓存命中 token 合计（bucket.CH）。
	CacheHit int64 `json:"cache_hit"`
	// CacheMiss 前缀缓存未命中 token 合计。见 bucketCacheMiss 的回退口径。
	CacheMiss int64 `json:"cache_miss"`
	// Completion 输出（completion）token 合计。
	Completion int64 `json:"completion"`
	// Total 上游报告的 total tokens 合计（缺失时按 prompt+completion 兜底）。
	Total int64 `json:"total"`
}

// CostBreakdown 高峰 / 空闲各自的金额拆分（取整前，两位小数由 rounding 阶段
// 单独处理；两个分量相加才是全天，先各自取整再相加会出现分位对不上）。
type CostBreakdown struct {
	PeakAmount    float64 `json:"peak_amount"`
	OffPeakAmount float64 `json:"offpeak_amount"`
}

// DayCost GET /api/cost/today 的响应体（也是面板展示用的数据结构）。
// 字段名/顺序对齐约定，外部程序按此解析，不要随意改动。
type DayCost struct {
	Date      string  `json:"date"`       // 北京时间的日历日，"2026-10-23"
	Timezone  string  `json:"timezone"`   // 进程本地时区名，"Asia/Shanghai"
	Currency  string  `json:"currency"`   // "CNY"
	Amount    float64 `json:"amount"`     // 四舍五入到两位小数
	AmountStr string  `json:"amount_str"` // 两位小数的字符串形式（"12.34"）
	// AmountRaw 未取整的原始金额。json:"-" 是刻意的：响应字段表是给其他程序解析的
	// 契约，多一个同义字段只会让人猜该用哪个；原始值留在进程内（面板可用于
	// 悬停明细、测试用于精确断言）。
	AmountRaw   float64       `json:"-"`
	Tokens      CostTokens    `json:"tokens"`
	Breakdown   CostBreakdown `json:"breakdown"`
	Pricing     Pricing       `json:"pricing"`
	Note        string        `json:"note"`
	GeneratedAt string        `json:"generated_at"` // RFC3339（带本地偏移）
}

// CostNote 响应里的口径说明（给解析方看的一句话）。
const CostNote = "当天全部模型按 DeepSeek V4.1 Flash 官方价折算（空闲/高峰分时段计价）"

// IsPeakHour 判断某个小时桶属于高峰还是空闲计价时段。
//
// 规则（北京时间，**左闭右开**）：
//   - 周一至周五 [09:00,12:00) 与 [14:00,18:00) → 高峰
//   - 其余时刻（含 12:00 整点、18:00 整点起的整个小时、夜间、周末全天）→ 空闲
//   - extraOffPeak（"YYYY-MM-DD" 集合）命中的日历日 → **全天空闲**，优先于
//     星期与时刻判断（法定节假日/调休）
//
// 边界为什么是左闭右开：官方把时段写成 "09:00–12:00"，12:00 这一整点起已属于
// 午间空闲（12:00–14:00 官方列为空闲）。桶的粒度恰好是一小时，所以
// "左闭右开"在小时桶上等价于"09/10/11 点高峰，12/13 点空闲，14/15/16/17 点
// 高峰，18 点起空闲"，不会出现半价半高峰的歧义。
//
// t 传桶的起始时刻（本地或任意时区都可以，内部先换算到北京时间再判断）。
func IsPeakHour(t time.Time, extraOffPeak map[string]bool) bool {
	bt := t.In(beijingZone)
	if extraOffPeak[bt.Format(dayLayout)] {
		return false
	}
	switch bt.Weekday() {
	case time.Saturday, time.Sunday:
		return false
	}
	switch h := bt.Hour(); {
	case h >= 9 && h < 12:
		return true
	case h >= 14 && h < 18:
		return true
	default:
		return false
	}
}

// bucketCacheMiss 取一个桶的「缓存未命中输入」token 数。
//
// 优先用 CM（上游明确回报的前缀缓存未命中累计）。CM 为 0 或缺失时回退
// PT-CH，原因：CM 是后加的字段（fileVersion 4 起才累计），**老桶里永远是 0**，
// 而老桶的 PT 是含缓存命中的总量——直接按 0 处理会把整段历史输入算成免费，
// 让金额系统性偏低。回退口径把"命中之外的部分"视为未命中，与 DeepSeek
// 的计费定义一致。
//
// 钳到 ≥0：上游个别情况下 CH 可能大于 PT（跨字段口径不一致/脏数据），
// 相减会得到负数，进而算出负金额——对外接口绝不能吐负数，故钳 0。
func bucketCacheMiss(b *bucket) int64 {
	if b.CM > 0 {
		return b.CM
	}
	if miss := b.PT - b.CH; miss > 0 {
		return miss
	}
	return 0
}

// bucketTotal 取桶的 total tokens：优先用上游报的 TT，缺失（0）时用 PT+CT 兜底，
// 保证 tokens.total 不会在只有 prompt/completion 的桶上显示 0。
func bucketTotal(b *bucket) int64 {
	if b.TT > 0 {
		return b.TT
	}
	return b.PT + b.CT
}

// TimezoneName 返回进程本地时区的可读名（容器 TZ=Asia/Shanghai 时即
// "Asia/Shanghai"）。
//
// 为什么需要兜底：时区未配置时 time.Local 的 String() 是 "Local"，对外没有
// 任何信息量——调用方看到 "Local" 无从判断日期按哪个偏移切。此时退回
// "UTC+HH:MM" 形式的固定偏移名，至少把真实偏移讲清楚。
func TimezoneName() string {
	if name := time.Local.String(); name != "" && name != "Local" {
		return name
	}
	_, off := time.Now().Zone()
	sign := "+"
	if off < 0 {
		sign, off = "-", -off
	}
	return fmt.Sprintf("UTC%s%02d:%02d", sign, off/3600, (off%3600)/60)
}

// CostForDay 聚合 day 当天（进程本地日历日）的全部小时桶，按峰谷分别计价后合计。
//
// 只取 "h:<day>T*" 前缀的桶：
//   - 日桶（"d:<day>"）**不参与**——同一份用量在跨粒度时可能同时存在 h: 与 d:
//     形态（Rollup 折叠只在 90 天前发生，但脏数据/导入数据不保证），两边都取
//     会重复计数；
//   - 其它日期的桶按前缀天然排除。
//
// 纯内存只读：加锁只为拷一份桶快照，聚合在锁外做，不阻塞 Add。
func (r *Recorder) CostForDay(day time.Time, p Pricing) DayCost {
	p = p.normalize()
	out := DayCost{
		Date:        day.Format(dayLayout),
		Timezone:    TimezoneName(),
		Currency:    pricingCurrency,
		Pricing:     p,
		Note:        CostNote,
		GeneratedAt: time.Now().Format(time.RFC3339),
	}
	if r == nil {
		out.AmountStr = formatAmount(0)
		return out
	}

	prefix := "h:" + out.Date + "T"
	extra := p.extraOffPeakSet()

	r.mu.Lock()
	bs := make([]bucket, 0, len(r.buckets))
	for _, b := range r.buckets {
		if strings.HasPrefix(b.Scope, prefix) {
			bs = append(bs, *b)
		}
	}
	r.mu.Unlock()

	var peak, offpeak float64
	for i := range bs {
		b := &bs[i]
		// 桶起点解析失败（脏 scope）直接跳过：宁可少数一元，也不猜时刻导致
		// 峰谷错判（错判会把金额算高一倍）。
		ts, err := time.ParseInLocation(hourLayout, strings.TrimPrefix(b.Scope, "h:"), time.Local)
		if err != nil {
			continue
		}
		hit, miss, comp := b.CH, bucketCacheMiss(b), b.CT

		out.Tokens.Prompt += b.PT
		out.Tokens.CacheHit += hit
		out.Tokens.CacheMiss += miss
		out.Tokens.Completion += comp
		out.Tokens.Total += bucketTotal(b)

		if IsPeakHour(ts, extra) {
			peak += tieredCost(hit, miss, comp,
				p.PeakInputCacheHit, p.PeakInputCacheMiss, p.PeakOutput)
		} else {
			offpeak += tieredCost(hit, miss, comp,
				p.OffPeakInputCacheHit, p.OffPeakInputCacheMiss, p.OffPeakOutput)
		}
	}

	out.Breakdown = CostBreakdown{PeakAmount: peak, OffPeakAmount: offpeak}
	out.AmountRaw = peak + offpeak
	// 四舍五入到两位小数。用 math.Round 而不是字符串截断：截断会把
	// 12.349 变 12.34、把 12.345 变 12.34（既不对也不单调），Round 是银行家
	// 之外的标准四舍五入，与"保留两位小数"的直觉一致。
	out.Amount = math.Round(out.AmountRaw*100) / 100
	out.AmountStr = formatAmount(out.Amount)
	return out
}

// CostToday 是 CostForDay(now) 的糖：接口与面板都用这个入口。
func (r *Recorder) CostToday(p Pricing) DayCost { return r.CostForDay(time.Now(), p) }

// tieredCost 三档单价折算金额（元）。分母 1e6 是「元 / 百万 tokens」的单位换算。
// 单独抽出来是为了让峰谷两条分支共用同一套算式——单价不同、口径必须完全一致。
func tieredCost(cacheHit, cacheMiss, completion int64, hitPrice, missPrice, outPrice float64) float64 {
	return (float64(cacheHit)*hitPrice +
		float64(cacheMiss)*missPrice +
		float64(completion)*outPrice) / 1_000_000
}

// formatAmount 固定两位小数的金额字符串（"12.34" / "0.00"）。
func formatAmount(v float64) string {
	return strconv.FormatFloat(v, 'f', 2, 64)
}
