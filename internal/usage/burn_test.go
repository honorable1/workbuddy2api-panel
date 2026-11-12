package usage

import (
	"math"
	"testing"
	"time"
)

// TestBurnRateWindow 钉住「速率分母」口径：速率必须用窗口实际覆盖时长折算，
// 而不是请求的 hours——窗口起点被对齐到整点，两者能差近一个小时。
// 同时钉住窗口外的桶不算进来（否则「近 24 小时」会把上周的消耗也摊进速率）。
func TestBurnRateWindow(t *testing.T) {
	r := New("")
	now := time.Now()
	// 窗口内：glm-5.2 三次共 60 积分
	for i := 0; i < 2; i++ {
		r.Add(now.Add(-2*time.Hour), "cn", "u1", "glm-5.2", Delta{Credit: 20, HasCredit: true}, true)
	}
	r.Add(now.Add(-1*time.Hour), "cn", "u1", "glm-5.2", Delta{Credit: 20, HasCredit: true}, true)
	// 窗口外：48 小时前的一大笔，不能计入 24h 速率
	r.Add(now.Add(-48*time.Hour), "cn", "u1", "glm-5.2", Delta{Credit: 999, HasCredit: true}, true)
	// 另一个域 + 另一个模型：必须独立成行，不能跨域合并
	r.Add(now.Add(-1*time.Hour), "global", "u2", "claude-4.6", Delta{Credit: 5, HasCredit: true}, true)
	// 失败尝试：没有 usage、不计积分，但要让 Errors 可区分——样本占比的分母
	// 用的是「成功数」，把失败算进分母会让正常模型长期看起来样本不足。
	r.Add(now.Add(-time.Hour), "cn", "u1", "glm-5.2", Delta{}, false)

	rows, hours := r.BurnRate(Window{Hours: 24})
	if hours < 22.9 || hours > 24.1 {
		t.Fatalf("hours=%v, want ≈24（整点对齐后的真实跨度）", hours)
	}
	byKey := map[string]ModelBurn{}
	for _, row := range rows {
		byKey[row.Realm+"/"+row.Model] = row
	}
	cn := byKey["cn/glm-5.2"]
	if cn.Credits != 60 || cn.Requests != 4 || cn.Errors != 1 || cn.CreditSamples != 3 {
		t.Fatalf("cn/glm-5.2 = %+v, want credits=60 requests=4 errors=1 samples=3", cn)
	}
	if want := 60 / hours; math.Abs(cn.CreditsPerHour-want) > 1e-9 {
		t.Fatalf("credits_per_hour=%v want %v", cn.CreditsPerHour, want)
	}
	if g := byKey["global/claude-4.6"]; g.Credits != 5 || g.Realm != "global" {
		t.Fatalf("global/claude-4.6 = %+v, want credits=5 且不并入 cn", g)
	}
}

// TestBurnRateAllHistory 全部历史（无窗口）时，分母取「最早桶 → 现在」的跨度。
// 若按 1 小时折算，历史消耗会被放大成几十倍的虚假速率，续航直接算成几分钟。
func TestBurnRateAllHistory(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now.Add(-48*time.Hour), "cn", "u1", "glm-5.2", Delta{Credit: 100, HasCredit: true}, true)
	r.Add(now.Add(-1*time.Hour), "cn", "u1", "glm-5.2", Delta{Credit: 100, HasCredit: true}, true)

	rows, hours := r.BurnRate(Window{})
	if len(rows) != 1 {
		t.Fatalf("rows=%d, want 1", len(rows))
	}
	if hours < 47 || hours > 49 {
		t.Fatalf("hours=%v, want ≈48", hours)
	}
	if rows[0].Credits != 200 {
		t.Fatalf("credits=%v, want 200", rows[0].Credits)
	}
	if rows[0].CreditsPerHour >= 100 {
		t.Fatalf("credits_per_hour=%v 明显偏高：疑似按 1 小时折算", rows[0].CreditsPerHour)
	}
}

// TestBurnRateNoCredit 有请求、无积分观测时速率恒 0 且样本为 0——调用方必须
// 靠 Requests 与 CreditSamples 的差值区分「免费」与「没观测」，不能只看速率。
func TestBurnRateNoCredit(t *testing.T) {
	r := New("")
	r.Add(time.Now(), "cn", "u1", "glm-5.2", Delta{PromptTokens: 10, HasPromptTokens: true}, true)
	rows, _ := r.BurnRate(Window{Hours: 24})
	if len(rows) != 1 {
		t.Fatalf("rows=%d, want 1", len(rows))
	}
	if rows[0].Credits != 0 || rows[0].CreditSamples != 0 || rows[0].CreditsPerHour != 0 {
		t.Fatalf("row=%+v, want 全零积分字段", rows[0])
	}
	if rows[0].Requests != 1 {
		t.Fatalf("requests=%d, want 1（无积分也要计请求数）", rows[0].Requests)
	}
}

// TestBurnRateNil 未装配用量记录器时不得 panic（面板 /model_eta 会先判 nil，
// 但 Snapshot 侧的 nil 容忍是既有约定，这里保持一致）。
func TestBurnRateNil(t *testing.T) {
	var r *Recorder
	if rows, hours := r.BurnRate(Window{Hours: 24}); rows != nil || hours != 0 {
		t.Fatalf("nil recorder 返回 rows=%v hours=%v", rows, hours)
	}
}
