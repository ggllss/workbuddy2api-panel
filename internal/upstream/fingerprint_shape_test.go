// fingerprint_shape_test.go 设备/会话标识与请求 id 的**形态**约束。
//
// 为什么专门测形态：风控不只看「有没有发 machineId」，也看「发的像不像真客户端」。
// 真实客户端的 machineId 取自 OS 硬件 UUID、sessionId 取自每进程一个 randomUUID，
// 两者都是 8-4-4-4-12 的 36 字符 UUID（版本位 4、变体位 10xx）；而请求 id 是
// 与账号无关的 32 位随机 hex。此前本仓发的是 36 位**无连字符** hex 和「账号派生
// 前缀 + 微秒」的请求 id——两者形态都对不上，本身就是可被识别的网关特征
// （参考实现 wb_fingerprint.py 同款结论）。
package upstream

import (
	"regexp"
	"testing"
)

// uuidV4Re 真实客户端 machineId / sessionId 的形态：8-4-4-4-12，版本位 4、变体位 a。
var uuidV4Re = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-a[0-9a-f]{3}-[0-9a-f]{12}$`)

func TestDeriveAccountStableIDIsUUIDv4Shape(t *testing.T) {
	for _, purpose := range []string{"machine", "session"} {
		got := deriveAccountStableID("21711c1f-0d9d-4e82-b1f2-b73021a6a72a", purpose)
		if !uuidV4Re.MatchString(got) {
			t.Errorf("purpose=%s 的派生值 %q 不是 UUIDv4 形态（裸 hex 本身就是网关特征）", purpose, got)
		}
	}
	// 幂等：同一账号同一用途必须恒定（跨重启稳定，不随进程变）。
	uid := "998eec9d-b85a-4d97-9ac9-95e71c9a069a"
	if a, b := deriveAccountStableID(uid, "machine"), deriveAccountStableID(uid, "machine"); a != b {
		t.Errorf("同账号同用途派生不稳定：%q vs %q（随机机器码会触发上游风控）", a, b)
	}
	// 账号间互异：不同账号不得共用同一台「虚拟设备」（那是把账号池串起来的关联键）。
	if deriveAccountStableID(uid, "machine") == deriveAccountStableID("21711c1f-0d9d-4e82-b1f2-b73021a6a72a", "machine") {
		t.Error("不同账号派生出相同的 machineId——账号池被关联成同一台设备")
	}
	// 用途隔离：同一账号的 machine 与 session 必须是两个不同标识。
	if deriveAccountStableID(uid, "machine") == deriveAccountStableID(uid, "session") {
		t.Error("machine 与 session 派生值相同——设备级与会话级标识未隔离")
	}
}

func TestUUIDV4ShapeForcesVersionAndVariant(t *testing.T) {
	// 全 0 的 32 hex 必须被改写成合法 UUIDv4 形态（版本位/变体位是强制写入的）。
	got := uuidV4Shape("00000000000000000000000000000000")
	if got != "00000000-0000-4000-a000-000000000000" {
		t.Errorf("uuidV4Shape 未强制版本位/变体位：%q", got)
	}
	// 入参不足 32 hex 时原样返回，不 panic、不伪造。
	if got := uuidV4Shape("abc"); got != "abc" {
		t.Errorf("短入参应原样返回，got %q", got)
	}
}

func TestRequestIDIsRandom32Hex(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{32}$`)
	seen := make(map[string]bool, 200)
	for i := 0; i < 200; i++ {
		id := newRequestID()
		if !re.MatchString(id) {
			t.Fatalf("请求 id %q 不是 32 位小写 hex（真实客户端形态）", id)
		}
		if seen[id] {
			t.Fatalf("请求 id 重复：%q（每条必须独立随机）", id)
		}
		seen[id] = true
	}
}
