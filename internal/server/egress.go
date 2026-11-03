// egress.go 出口级退避在 handler 侧的挂载：前置闸 + 记账 + 成功清零。
//
// 分工：错误归责（哪些错记出口、哪些记账号）在 upstream.FaultOfKind，退避状态机
// 在 internal/egress，本文件只负责「在轮转路径的哪个点上用它们」。三者分离让
// 「改判定口径」与「改退避尺度」互不影响。
package server

import (
	"context"
	"log"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/egress"
)

// egressEnabled 报告出口级退避是否启用（Config.Egress 为 nil 或开关关闭时为
// false，调用方据此逐字保留改造前行为）。
func (h *Handler) egressEnabled() bool { return h.cfg.Egress.Enabled() }

// egressKey 当前请求的出口键。当前部署只有直连一个出口（不做代理池），故恒为
// egress.Direct；将来加代理时按账号的 proxy 派生（egress.KeyFor），同一代理下的
// 账号自动共用一个计数器——这正是「按出口键设计」的目的。
func (h *Handler) egressKey() egress.Key { return egress.Direct }

// egressAwait 出口退避前置闸：退避窗口内**不发上游请求**，先等满剩余窗口。
//
// 为什么是「等」而不是「立刻拒绝」：退避上限 15 秒（账号级熔断是分钟级），而
// 拒绝会让客户端立刻重试，反而把请求密度推高——那正是要消掉的机器特征。等待
// 有界且可取消：客户端断连 / 优雅停机时立即放弃。
//
// 返回 false = ctx 已取消（客户端断连或停机），调用方终止轮转。
func (h *Handler) egressAwait(ctx context.Context) bool {
	g := h.cfg.Egress
	if !g.Enabled() {
		return ctx.Err() == nil
	}
	d := g.Wait(h.egressKey())
	if d <= 0 {
		return ctx.Err() == nil
	}
	log.Printf("WARN: [server] egress backoff: waiting %s before the next upstream attempt (no request is sent while backing off)",
		d.Round(time.Millisecond))
	return sleepCtx(ctx, d)
}

// noteEgressFailure 把一次出口/链路类失败记在**出口**上（而不是账号上），返回
// 是否已按出口记账。false = 出口退避未启用，调用方应走既有的账号级处置路径。
//
// 只记账不等待：等待集中由下一次 attempt 前的 egressAwait 承担。失败点之后紧跟
// 换号退避（rotateBackoff），两段等待叠加等于白等；而且「退避期间不产生上游
// 请求」这条不变量只需要在一处保证，分散到各失败点反而容易漏。
func (h *Handler) noteEgressFailure(what string) bool {
	g := h.cfg.Egress
	if !g.Enabled() {
		return false
	}
	key := h.egressKey()
	wait := g.Note(key)
	log.Printf("WARN: [server] egress transient failure (%s): egress=%s streak=%d, backing off %s (account not penalized)",
		what, key, g.Streak(key), wait.Round(time.Millisecond))
	return true
}

// egressSuccess 这一段出口成功了一次：清零连续计数与退避（与账号级 NoteSuccess
// 同一规矩——成功即清零；抖动是瞬时的，出口一旦通了就不该再背着上一次的阶梯）。
func (h *Handler) egressSuccess() {
	if !h.egressEnabled() {
		return
	}
	h.cfg.Egress.NoteSuccess(h.egressKey())
}
