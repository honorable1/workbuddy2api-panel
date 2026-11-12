package panel

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/credithist"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// TestEtaHours 钉住续航钳制的算术边界。这是整个功能唯一「算错就会给出危险
// 误导」的地方：把到期作废当成烧完，运维会以为还有额度可用而实际已经清零。
func TestEtaHours(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.Local)

	cases := []struct {
		name     string
		pool     int64
		burn     float64
		buckets  []expiryPoint
		wantHour float64
		wantCap  bool
	}{
		{name: "无消耗不外推", pool: 100, burn: 0, wantHour: 0, wantCap: false},
		{name: "无余额", pool: 0, burn: 10, wantHour: 0, wantCap: false},
		{
			name: "无到期批次：纯烧穿", pool: 100, burn: 10,
			wantHour: 10, wantCap: false,
		},
		{
			// 4h 时烧掉 4 剩 96，其中 10 到期作废 → 剩 86，再烧 86h。
			name: "批次到期但不够大：烧穿在后", pool: 100, burn: 1,
			buckets:  []expiryPoint{{at: now.Add(4 * time.Hour), remain: 10}},
			wantHour: 90, wantCap: false,
		},
		{
			name: "批次到期把余额一次清零：到期即耗尽", pool: 100, burn: 10,
			buckets:  []expiryPoint{{at: now.Add(4 * time.Hour), remain: 100}},
			wantHour: 4, wantCap: true,
		},
		{
			name: "批次大于余额：扣到 0 为止，不能算成负数", pool: 100, burn: 1,
			buckets:  []expiryPoint{{at: now.Add(5 * time.Hour), remain: 500}},
			wantHour: 5, wantCap: true,
		},
		{
			name: "已过期批次跳过（不重复扣）", pool: 100, burn: 10,
			buckets:  []expiryPoint{{at: now.Add(-time.Hour), remain: 100}},
			wantHour: 10, wantCap: false,
		},
		{
			name: "多批次累计钳制", pool: 100, burn: 5,
			buckets: []expiryPoint{
				{at: now.Add(2 * time.Hour), remain: 20}, // 剩 70
				{at: now.Add(6 * time.Hour), remain: 70}, // 烧 20 后剩 50，清 50 → 0
			},
			wantHour: 6, wantCap: true,
		},
	}
	for _, c := range cases {
		got, cap := etaHours(c.pool, c.burn, now, c.buckets)
		if math.Abs(got-c.wantHour) > 1e-6 || cap != c.wantCap {
			t.Errorf("%s: etaHours=%v cap=%v, want %v/%v", c.name, got, cap, c.wantHour, c.wantCap)
		}
	}
}

// newEtaPanel 造一个只有本地状态（池快照 + 用量账本）的面板，不打上游。
func newEtaPanel(credits int64, buckets []pool.ExpiryBucket) (*Panel, *pool.Pool, *usage.Recorder) {
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", Nickname: "n1"})
	pl.SetCreditsSnapshot("u1", pool.CreditsSnapshot{Remain: credits, Total: credits, ExpiryBuckets: buckets})
	rec := usage.New("")
	return New(Config{Version: "test", Pool: pl, Usage: rec}), pl, rec
}

func getEta(t *testing.T, p *Panel, query string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/model_eta"+query, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("model_eta status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

// findEtaRow 按裸模型名取行（测试不假设账号属于哪个域）。
func findEtaRow(t *testing.T, resp map[string]any, model string) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(resp["rows"])
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("decode rows: %v", err)
	}
	for _, r := range rows {
		if r["model"] == model {
			return r
		}
	}
	t.Fatalf("rows 里没有模型 %q: %v", model, rows)
	return nil
}

// TestModelEtaEstimates 端到端：余额 1000、窗口内消耗 20 → ETA ≈ 余额/速率。
func TestModelEtaEstimates(t *testing.T) {
	p, _, rec := newEtaPanel(1000, nil)
	now := time.Now()
	for i := 0; i < 4; i++ {
		rec.Add(now.Add(-time.Hour), "cn", "u1", "glm-5.2", usage.Delta{Credit: 5, HasCredit: true}, true)
	}

	resp := getEta(t, p, "?hours=24")
	eff, _ := resp["effective_hours"].(float64)
	if eff < 22.9 || eff > 24.1 {
		t.Fatalf("effective_hours=%v, want ≈24", eff)
	}
	row := findEtaRow(t, resp, "glm-5.2")
	if row["state"] != "ok" {
		t.Fatalf("state=%v, want ok（row=%v）", row["state"], row)
	}
	if row["pool_credits"].(float64) != 1000 || row["credits"].(float64) != 20 {
		t.Fatalf("pool/credits=%v/%v, want 1000/20", row["pool_credits"], row["credits"])
	}
	burn := 20 / eff
	if got := row["burn_per_hour"].(float64); math.Abs(got-burn) > 1e-6 {
		t.Fatalf("burn_per_hour=%v want %v", got, burn)
	}
	if got, want := row["eta_hours"].(float64), 1000/burn; math.Abs(got-want) > 1e-6 {
		t.Fatalf("eta_hours=%v want %v", got, want)
	}
	if row["eta_until"] == nil {
		t.Fatal("eta_until 缺失：前端无法给出具体时刻")
	}
}

// TestModelEtaExpiryCapped 余额其实会被到期清零时，ETA 必须落在到期时刻并标
// expiry_capped——否则「还能用 40 天」的假象会让人放着积分过期。
func TestModelEtaExpiryCapped(t *testing.T) {
	now := time.Now()
	at := now.Add(10 * time.Hour)
	p, _, rec := newEtaPanel(1000, []pool.ExpiryBucket{
		{Date: at.Format("2006-01-02"), ExpireAt: at, Remain: 1000},
	})
	for i := 0; i < 4; i++ {
		rec.Add(now.Add(-time.Hour), "cn", "u1", "glm-5.2", usage.Delta{Credit: 5, HasCredit: true}, true)
	}

	row := findEtaRow(t, getEta(t, p, "?hours=24"), "glm-5.2")
	if row["expiry_capped"] != true {
		t.Fatalf("expiry_capped 未置位（row=%v）", row)
	}
	if got := row["eta_hours"].(float64); math.Abs(got-10) > 0.05 {
		t.Fatalf("eta_hours=%v, want ≈10（到期时刻）", got)
	}
}

// TestModelEtaLockOverrides 模型整池限流时，状态必须是 locked 而不是「还能用
// 多少天」——那一刻的续航数字没有意义，倒计时才是运维要的答案。
func TestModelEtaLockOverrides(t *testing.T) {
	p, pl, rec := newEtaPanel(1000, nil)
	now := time.Now()
	for i := 0; i < 4; i++ {
		rec.Add(now.Add(-time.Hour), "cn", "u1", "glm-5.2", usage.Delta{Credit: 5, HasCredit: true}, true)
	}
	pl.CooldownSoftForModel("u1", time.Minute, now.Add(30*time.Minute), "glm-5.2", "6004 model rate limit")

	row := findEtaRow(t, getEta(t, p, "?hours=24"), "glm-5.2")
	if row["state"] != "locked" {
		t.Fatalf("state=%v, want locked（row=%v）", row["state"], row)
	}
	if row["unlock_at"] == nil {
		t.Fatal("locked 行必须带 unlock_at 倒计时")
	}
	if row["eta_hours"] != nil {
		t.Fatalf("locked 行仍给出 eta_hours=%v：会与「此刻不可用」自相矛盾", row["eta_hours"])
	}
}

// TestModelEtaFreeAndIdle 有请求无积分 → free（限免/未计费，不能显示 ∞）；
// 有余额无请求 → 不出行（没有速率可算，凭空给个「永久」是错的）。
func TestModelEtaFreeAndIdle(t *testing.T) {
	p, _, rec := newEtaPanel(1000, nil)
	rec.Add(time.Now().Add(-time.Hour), "cn", "u1", "free-model", usage.Delta{PromptTokens: 100, HasPromptTokens: true}, true)

	resp := getEta(t, p, "?hours=24")
	row := findEtaRow(t, resp, "free-model")
	if row["state"] != "free" {
		t.Fatalf("state=%v, want free（row=%v）", row["state"], row)
	}
	if row["eta_hours"] != nil {
		t.Fatalf("free 行不该有 eta_hours=%v", row["eta_hours"])
	}
	if rc, ok := resp["realm_credits"].(map[string]any); !ok || len(rc) == 0 {
		t.Fatalf("realm_credits=%v, want 非空（前端要展示可动用余额）", resp["realm_credits"])
	}
}

// TestModelEtaRequiresUsage 未装配用量记录器时返回 501，与 /usage 一致。
func TestModelEtaRequiresUsage(t *testing.T) {
	pl := pool.New("")
	p := New(Config{Version: "test", Pool: pl})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/model_eta", nil))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status=%d, want 501", rec.Code)
	}
}

// newEtaPanelWithLedger 造一个带积分流水账本的面板：余额 1000、账号 uid=u1。
func newEtaPanelWithLedger(t *testing.T, credits int64) (*Panel, *usage.Recorder, *credithist.Ledger) {
	t.Helper()
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", Nickname: "n1"})
	pl.SetCreditsSnapshot("u1", pool.CreditsSnapshot{Remain: credits, Total: credits})
	rec := usage.New("")
	led := credithist.New("", 0)
	return New(Config{Version: "test", Pool: pl, Usage: rec, CreditHistory: led}), rec, led
}

// TestModelEtaBalanceFallback 上游不回模型级扣费（usage.credit 恒为 0，本实例
// 实测如此）时，只要有余额变动流水，就必须给出全池口径的参考续航，而不是
// 一句错的「免费中」——余额在掉就说明在花钱。
func TestModelEtaBalanceFallback(t *testing.T) {
	p, rec, led := newEtaPanelWithLedger(t, 1000)
	// 观测两次余额：第一次建基线，第二次生成一条 -10 的消耗流水。
	led.Observe("u1", 1000)
	led.Observe("u1", 990)
	// 请求本身没带 credit 观测（HasCredit=false），模型级速率必为 0。
	rec.Add(time.Now().Add(-time.Hour), "cn", "u1", "glm-5.2", usage.Delta{PromptTokens: 100, HasPromptTokens: true}, true)

	resp := getEta(t, p, "?hours=24")
	row := findEtaRow(t, resp, "glm-5.2")
	if row["state"] != "pool" {
		t.Fatalf("state=%v, want pool（有余额流水时应降级而非报免费）row=%v", row["state"], row)
	}
	if row["eta_source"] != "balance" {
		t.Fatalf("eta_source=%v, want balance", row["eta_source"])
	}
	if got := row["pool_consumed"].(float64); got != 10 {
		t.Fatalf("pool_consumed=%v, want 10", got)
	}
	// 分母是完整窗口 24h（余额刷新是分钟级的，窗口内没流水=没花钱）。
	wantHours := 1000 / (10.0 / 24.0)
	if got := row["eta_hours"].(float64); math.Abs(got-wantHours) > 0.5 {
		t.Fatalf("eta_hours=%v, want ≈%v", got, wantHours)
	}
	b, ok := resp["balance"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺少 balance 汇总: %v", resp)
	}
	if b["consumed"].(float64) != 10 || b["pool_credits"].(float64) != 1000 {
		t.Fatalf("balance=%v", b)
	}
}

// TestModelEtaBalanceGainCoversBurn 签到补回的积分大于消耗时净消耗为 0，
// 不能外推成「还能撑多久」——此时正确的答案是「余额在涨」，而不是「免费中」：
// 消耗真实发生了，只是补充更快。
func TestModelEtaBalanceGainCoversBurn(t *testing.T) {
	p, rec, led := newEtaPanelWithLedger(t, 1000)
	led.Observe("u1", 1000)
	led.Observe("u1", 1050) // 签到 +50
	led.Observe("u1", 1040) // 消耗 -10
	rec.Add(time.Now().Add(-time.Hour), "cn", "u1", "glm-5.2", usage.Delta{PromptTokens: 100, HasPromptTokens: true}, true)

	row := findEtaRow(t, getEta(t, p, "?hours=24"), "glm-5.2")
	if row["state"] != "growing" {
		t.Fatalf("state=%v, want growing（净消耗≤0 时不该给续航天数）row=%v", row["state"], row)
	}
	if row["eta_hours"] != nil {
		t.Fatalf("growing 行不该有 eta_hours=%v", row["eta_hours"])
	}
	if row["pool_consumed"].(float64) != 10 || row["pool_earned"].(float64) != 50 {
		t.Fatalf("pool_consumed/earned=%v/%v, want 10/50", row["pool_consumed"], row["pool_earned"])
	}
}

// TestModelEtaBalanceIgnoresOtherRealm 流水只记 uid，域归属必须由账号池决定：
// 已不在池里的 uid 的变动不能算进任何域的消耗。
func TestModelEtaBalanceIgnoresOtherRealm(t *testing.T) {
	p, rec, led := newEtaPanelWithLedger(t, 1000)
	led.Observe("u1", 1000)
	led.Observe("u1", 990)
	led.Observe("removed-uid", 5000)
	led.Observe("removed-uid", 4000) // 1000 消耗，但该账号已不在池里
	rec.Add(time.Now().Add(-time.Hour), "cn", "u1", "glm-5.2", usage.Delta{PromptTokens: 100, HasPromptTokens: true}, true)

	row := findEtaRow(t, getEta(t, p, "?hours=24"), "glm-5.2")
	if got := row["pool_consumed"].(float64); got != 10 {
		t.Fatalf("pool_consumed=%v, want 10（不在池里的账号不该计入）", got)
	}
}

// TestSeedCatalogRows 钉住目录铺行的三条口径：
//   - 只补 idle（目录不知道花不花钱，不能编续航）；
//   - 该域余额为 0 时铺 empty（没余额的域根本用不了，别拿「无请求」糊过去）；
//   - 已存在的 key 不重复铺（用量/锁行优先级高于目录行）。
func TestSeedCatalogRows(t *testing.T) {
	rows := []modelEtaRow{{Model: "glm-5.2", Realm: "cn", State: "ok", Credits: 12}}
	seen := map[string]struct{}{"cn\x1fglm-5.2": {}}

	seedCatalogRows(&rows, seen, "cn", map[string]string{"glm-5.2": "0.79", "deepseek-v4.1-flash": "0.11"}, 800)
	if len(rows) != 2 {
		t.Fatalf("rows=%d, want 2（已存在的模型不应重复铺）", len(rows))
	}
	if rows[0].State != "ok" || rows[0].Credits != 12 {
		t.Fatalf("已有行被覆盖: %+v", rows[0])
	}
	got := rows[1]
	if got.Model != "deepseek-v4.1-flash" || got.State != "idle" || got.CreditRate != "0.11" {
		t.Fatalf("目录行=%+v，应为 idle 且带倍率", got)
	}
	if got.PoolCredits != 800 {
		t.Fatalf("pool_credits=%d, want 800", got.PoolCredits)
	}
	if got.EtaHours != 0 {
		t.Fatalf("目录行不该有续航数字: %+v", got)
	}

	// 余额为 0 的域：铺 empty 而不是 idle。
	empty := []modelEtaRow{}
	seedCatalogRows(&empty, map[string]struct{}{}, "global", map[string]string{"claude-4.6": "1"}, 0)
	if len(empty) != 1 || empty[0].State != "empty" {
		t.Fatalf("空余额域应铺 empty: %+v", empty)
	}

	// 空目录：什么都不做（不能凭空造行）。
	none := []modelEtaRow{}
	seedCatalogRows(&none, map[string]struct{}{}, "cn", nil, 100)
	if len(none) != 0 {
		t.Fatalf("空目录不该铺行: %+v", none)
	}
}

// TestModelEtaCatalogSeedsIdleRows 端到端：窗口内一条请求都没有时，只要模型目录
// 已预热，接口也必须把已知模型列出来（state=idle + 倍率），而不是返回 rows=[]。
// 这正是线上「模型可用性没有数据」的直接成因。
func TestModelEtaCatalogSeedsIdleRows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/console/enterprises/personal/models") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["glm-5.2","deepseek-v4.1-flash"]}],` +
				`"models":[{"id":"glm-5.2","credits":"0.79"},{"id":"deepseek-v4.1-flash","credits":"0.11"}]}}`))
			return
		}
		http.NotFound(w, r) // v3/config 404 → FetchModels 降级到企业端点
	}))
	defer srv.Close()

	up := upstream.New()
	up.ChatBaseCN = srv.URL
	if _, err := up.FetchModels(&auth.Auth{UID: "u1"}); err != nil {
		t.Fatalf("FetchModels: %v", err)
	}

	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", Nickname: "n1"})
	pl.SetCreditsSnapshot("u1", pool.CreditsSnapshot{Remain: 800, Total: 800})
	// 注意：Usage 账本为空——模拟「重启后还没有任何请求」。
	p := New(Config{Version: "test", Pool: pl, Upstream: up, Usage: usage.New("")})

	resp := getEta(t, p, "?hours=24")
	raw, _ := json.Marshal(resp["rows"])
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("decode rows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows=%d, want 2（目录里的两个模型都要出现）", len(rows))
	}
	row := findEtaRow(t, resp, "glm-5.2")
	if row["state"] != "idle" {
		t.Fatalf("state=%v, want idle", row["state"])
	}
	if row["credit_rate"] != "0.79" {
		t.Fatalf("credit_rate=%v, want 0.79", row["credit_rate"])
	}
	if row["eta_hours"] != nil {
		t.Fatalf("idle 行不该带 eta_hours: %v", row["eta_hours"])
	}
}
