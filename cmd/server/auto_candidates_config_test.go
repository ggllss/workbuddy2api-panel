// auto_candidates_config_test.go 全量 auto（auto:all）候选模型列表配置测试：
// 键缺席 = 默认空（功能关闭）/ 去空去重 / realm 前缀校验（非法项 fail fast）/
// 非法项在启动与面板保存两条路径上都被拒。
package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestAutoCandidatesDefaultOff 键缺席 → 空列表（功能关闭，行为与引入前一致）。
func TestAutoCandidatesDefaultOff(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if len(c.Auto.Candidates) != 0 {
		t.Errorf("candidates=%v want empty (default off)", c.Auto.Candidates)
	}
}

// TestAutoCandidatesParsedFromFile 显式配置覆盖默认，顺序原样保留（顺序即优先级）。
func TestAutoCandidatesParsedFromFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	body := `{"auto":{"candidates":["global:deepseek-v4.1-flash","cn:deepseek-v4.1-flash"]}}`
	if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"global:deepseek-v4.1-flash", "cn:deepseek-v4.1-flash"}
	if !reflect.DeepEqual(c.Auto.Candidates, want) {
		t.Errorf("candidates=%v want %v", c.Auto.Candidates, want)
	}
}

// TestNormalizeAutoCandidates 归一化：去空 / 去重（保留首次出现顺序）/ 裁剪首尾空白 /
// 双域同名模型互不去重（cn:x 与 global:x 是两个不同候选）。
func TestNormalizeAutoCandidates(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil", nil, []string{}},
		{"去空", []string{"", "   ", "\t"}, []string{}},
		{"去空保留有效项", []string{"", "cn:a", " "}, []string{"cn:a"}},
		{"去重保留首次顺序", []string{"global:b", "cn:a", "global:b", " cn:a "}, []string{"global:b", "cn:a"}},
		{"双域同名不合并", []string{"cn:a", "global:a"}, []string{"cn:a", "global:a"}},
		{"裁剪空白", []string{"  cn: a  "}, []string{"cn:a"}},
		{"模型名可含冒号", []string{"cn:vendor:model"}, []string{"cn:vendor:model"}},
	}
	for _, tc := range cases {
		got, err := normalizeAutoCandidates(tc.in)
		if err != nil {
			t.Errorf("%s: 意外错误 %v", tc.name, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

// TestNormalizeAutoCandidatesRejects 非法项 fail fast（不静默丢弃：面板勾了却没生效
// 会无从排查）。裸名不猜域——猜错会把请求打到另一个域端点。
func TestNormalizeAutoCandidatesRejects(t *testing.T) {
	cases := []struct {
		name    string
		in      []string
		wantSub string
	}{
		{"裸名缺前缀", []string{"glm-5.2"}, "缺少 realm 前缀"},
		{"非法前缀", []string{"us:glm-5.2"}, "realm 前缀非法"},
		{"大写前缀", []string{"CN:glm-5.2"}, "realm 前缀非法"},
		{"空模型名", []string{"cn:"}, "缺少模型名"},
		{"仅冒号", []string{":"}, "realm 前缀非法"},
		{"自指 auto:all", []string{"cn:auto:all"}, "自指"},
	}
	for _, tc := range cases {
		got, err := normalizeAutoCandidates(tc.in)
		if err == nil {
			t.Errorf("%s: 期望报错，得到 %v", tc.name, got)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantSub) {
			t.Errorf("%s: err=%v 应含 %q", tc.name, err, tc.wantSub)
		}
	}
}

// TestAutoCandidatesInvalidFailsLoad 配置文件里的非法项在启动/面板保存（同一
// ParseConfig 路径）就被拒，不会带着脏候选起来。
func TestAutoCandidatesInvalidFailsLoad(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	if err := os.WriteFile(fp, []byte(`{"auto":{"candidates":["glm-5.2"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(fp)
	if err == nil {
		t.Fatal("非法候选项应导致 Load 失败")
	}
	if !strings.Contains(err.Error(), "auto.candidates") {
		t.Errorf("err=%v 应点名字段 auto.candidates", err)
	}
}

// TestAutoCandidatesEmptyArrayClears 面板清空候选（提交 "candidates": []）必须真的
// 清空——否则「取消勾选后保存」看起来成功但配置里还是旧列表。
func TestAutoCandidatesEmptyArrayClears(t *testing.T) {
	c, err := ParseConfig([]byte(`{"auto":{"candidates":[]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Auto.Candidates) != 0 {
		t.Errorf("candidates=%v want empty", c.Auto.Candidates)
	}
}

// TestAutoCandidatesHotReloadNotRestartRequired 候选列表走 pool setter 热生效
// （saveConfig 里 SetAutoCandidates），不得出现在「需重启」清单里——否则面板会
// 让用户为一个已经生效的改动去重启进程。
func TestAutoCandidatesHotReloadNotRestartRequired(t *testing.T) {
	c, err := ParseConfig([]byte(`{"auto":{"candidates":["cn:a"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range restartRequiredFields(c) {
		if strings.Contains(f, "auto") {
			t.Errorf("restartRequiredFields 不该含 %q（auto.candidates 是热生效项）", f)
		}
	}
}
