package panel

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/credithist"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// 模型续航（issue: 各模型剩余可用时间）视图。
//
// 为什么需要「估算」而不是读一个字段：上游**没有模型级额度**。积分包挂在账号上
// （p_tcaca 包带 CapacityRemain / DeductionEndTime），模型条目里的 credits 是
// **倍率**（x0.29 这种），不是配额。所以「这个模型还能用多久」只能由
// 「该域剩余积分 ÷ 该模型近期消耗速率」外推，再被积分到期批次钳制。
//
// 两条数据链各自独立、精度也完全不同，故在同一个响应里分开表达：
//   - eta_*、burn_per_hour：**估算**，随请求结构变化，只回答「容量还剩多少」；
//   - locked / unlock_at：**精确**，来自上游 6004 的重置墙钟或本地退避截止
//     （pool.ModelLockView 的同一事实来源），回答「什么时候能再用」。
const (
	// etaDefaultHours 默认统计窗口。取 24h 而不是 1h：积分扣除是重尾分布，
	// 一条 1M token 请求能把小时速率拉高一个量级，短窗口的 ETA 会剧烈跳动。
	etaDefaultHours = 24
	// etaMaxHours 窗口上限 30 天：再长就跨进日桶（小时桶只留 90 天，但日桶
	// 折叠后粒度更粗），且「一个月前的用量」对当前容量判断已经没意义。
	etaMaxHours = 24 * 30
	// etaMinSamples 最少积分样本数。低于此值不外推——三两条请求的速率不是速率。
	etaMinSamples = 3
)

// expiryPoint 一个积分到期事件：at 时刻有 remain 积分失效。
// 只用于续航的钳制计算，按 at 升序喂给 etaHours。
type expiryPoint struct {
	at     time.Time
	remain int64
}

// etaHours 在「余额按 burn 匀速烧 + 到期批次按时消失」的假设下，算积分耗尽
// 需要多少小时；capped 表示耗尽发生在某个到期时刻（积分是「过期作废」而不是
// 「烧完」，两者对运维的含义不同：前者说明剩余额度根本用不掉）。
//
// burn<=0 或 pool<=0 时返回 0，由调用方先判状态——本函数不做语义判断，只做算术，
// 这样边界（批次大于余额、批次已过期、时钟漂移）都能在单测里逐条钉住。
//
// buckets 必须按 at 升序且只含未来事件；乱序会让「先扣哪个批次」算错。
func etaHours(poolCredits int64, burn float64, now time.Time, buckets []expiryPoint) (float64, bool) {
	if burn <= 0 || poolCredits <= 0 {
		return 0, false
	}
	remaining := float64(poolCredits)
	elapsed := 0.0
	for _, b := range buckets {
		dt := b.at.Sub(now).Hours()
		if dt <= elapsed {
			// 已过期或与上一个事件同刻：余额快照本就不该含它，跳过而不是扣两次。
			continue
		}
		// 到这个事件之前按速率烧掉的量。
		spend := burn * (dt - elapsed)
		if remaining <= spend {
			return elapsed + remaining/burn, false
		}
		remaining -= spend
		// 该批次作废。批次之和可能大于余额（快照与本表口径不一致时），
		// 最多扣到 0，不能让 remaining 变负后把 ETA 算成负数。
		gone := float64(b.remain)
		if gone > remaining {
			gone = remaining
		}
		remaining -= gone
		elapsed = dt
		if remaining <= 0 {
			return elapsed, true
		}
	}
	return elapsed + remaining/burn, false
}

// modelEtaRow 单个 (域, 模型) 的续航行。字段命名与 pool.ModelLockRow 对齐
// （model/realm/locked/servable/total/unlock_at），前端两张表可以共用渲染逻辑。
type modelEtaRow struct {
	Model string `json:"model"`
	Realm string `json:"realm"`
	// State 取值：
	//   ok          有速率、有余额、样本够 → eta_hours 可用
	//   free        有请求但没扣积分（限免 / 夜间免费 / 未计费）→ 不显示 ∞
	//   idle        窗口内没有该模型的请求 → 不外推
	//   empty       该域余额已为 0
	//   insufficient 积分样本太少或占比过低 → 速率只能算下限
	//   pool        上游未报模型级扣费（usage.credit 恒为 0），降级用账号余额
	//               变动流水外推——**参考值**，eta_source=balance
	//   growing     窗口内余额净增（签到/任务补的比花的多）→ 不会耗尽，但也不是
	//               「免费」（余额确实在扣），两侧数字见 pool_consumed / pool_earned
	//   locked      该模型整池被限流（pool.ModelLockView 同口径）
	//   starved     没号能服务，但不是模型自身被锁
	State         string  `json:"state"`
	BurnPerHour   float64 `json:"burn_per_hour"`
	Credits       float64 `json:"credits"` // 窗口内该模型已消耗积分
	CreditSamples int64   `json:"credit_samples"`
	Requests      int64   `json:"requests"`
	// CreditRate 目录里的当前倍率（相对价，如 "0.11"）。只有在倍率表里出现过的
	// 模型才有值——它是「这行是目录铺出来的」唯一的痕迹，前端据此说明为什么
	// 一行有模型名却没有任何用量数字。
	CreditRate  string    `json:"credit_rate,omitempty"`
	PoolCredits int64     `json:"pool_credits"` // 该域可路由账号的余额合计
	EtaHours    float64   `json:"eta_hours,omitempty"`
	EtaUntil    time.Time `json:"eta_until,omitempty"`
	// EtaSource 标明 eta_* 的来源口径：
	//   ""（空）模型级积分观测（最准，但要求上游回非零 usage.credit）
	//   balance 账号余额变动流水的全池口径（参考值，分不出模型）
	// 前端必须据此加「≈」前缀——两种 Precision 混在一起会让估算被当成配额。
	EtaSource    string  `json:"eta_source,omitempty"`
	PoolBurn     float64 `json:"pool_burn_per_hour,omitempty"` // 该域余额口径净消耗（积分/时）
	PoolConsumed int64   `json:"pool_consumed,omitempty"`      // 窗口内该域扣掉的积分
	PoolEarned   int64   `json:"pool_earned,omitempty"`        // 窗口内该域补回的积分（签到/奖励）
	// ExpiryCapped 为 true 表示「余额不是烧完的，是到期作废的」——此时加大用量
	// 也延长不了可用时间，唯一有效动作是把积分花掉或换号。
	ExpiryCapped  bool      `json:"expiry_capped,omitempty"`
	Locked        int       `json:"locked,omitempty"`
	Servable      int       `json:"servable,omitempty"`
	Total         int       `json:"total,omitempty"`
	UnlockAt      time.Time `json:"unlock_at,omitempty"`
	FullyUnlockAt time.Time `json:"fully_unlock_at,omitempty"`
	Reason        string    `json:"reason,omitempty"`
}

// modelEta 返回各模型的续航估算与限流倒计时。
//
// 只读本地状态（池快照 + 用量账本），**不打上游**：模型目录那条路要真实请求
// 上游 v3/config，几秒起步，不能给一个会被轮询的接口用。代价是「从未用过、
// 也没被限流」的模型不会出现在 rows 里——那种行没有任何可算的数，前端按
// 缺省「—」渲染即可。
func (p *Panel) modelEta(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Usage == nil {
		writeErr(w, http.StatusNotImplemented, "usage recorder not available")
		return
	}
	hours := etaDefaultHours
	if v := r.URL.Query().Get("hours"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			hours = n
		}
	}
	if hours > etaMaxHours {
		hours = etaMaxHours
	}

	now := time.Now()
	burns, effHours := p.cfg.Usage.BurnRate(usage.Window{Hours: hours})

	// 余额与到期批次按域聚合。禁用 / 暂停号不计：它们的积分不参与路由，
	// 计进来会把「能撑多久」算高（与 ModelLockView 的 Total 同口径）。
	poolByRealm := map[string]int64{}
	bucketsByRealm := map[string][]expiryPoint{}
	uidsByRealm := map[string]map[string]bool{}
	for _, s := range p.cfg.Pool.List() {
		if s.Disabled || s.Paused {
			continue
		}
		realm := s.Realm
		if realm == "" {
			realm = "cn"
		}
		poolByRealm[realm] += s.Credits
		if uidsByRealm[realm] == nil {
			uidsByRealm[realm] = map[string]bool{}
		}
		uidsByRealm[realm][s.UID] = true
		for _, b := range s.CreditsExpiryBuckets {
			if b.Remain <= 0 || !b.ExpireAt.After(now) {
				continue
			}
			bucketsByRealm[realm] = append(bucketsByRealm[realm], expiryPoint{at: b.ExpireAt, remain: b.Remain})
		}
	}
	for realm := range bucketsByRealm {
		bs := bucketsByRealm[realm]
		sort.Slice(bs, func(i, j int) bool { return bs[i].at.Before(bs[j].at) })
		bucketsByRealm[realm] = bs
	}

	// 余额口径的降级速率。上游 usage.credit 恒为 0 时（本实例实测），模型级速率
	// 永远是「免费中」；但账号余额确实在降，credit-history 逐次记了余额变动。
	// 按域汇总负 delta 即可得到真实消耗——代价是分不出模型，只能给全池口径。
	//
	// 分母取**完整窗口**而不是「首条变动→现在」：余额刷新是分钟级的，窗口内没有
	// 流水说明那段时间没花钱（不是没观测）。按首条变动起算会把一次 10 积分的小
	// 消耗放大成每小时数百，ETA 缩成几小时，比不给数更误导。
	drainByRealm := map[string]float64{}
	statsByRealm := map[string]credithist.Stats{}
	if p.cfg.CreditHistory != nil && hours > 0 {
		windowStart := now.Add(-time.Duration(hours) * time.Hour)
		for realm, uids := range uidsByRealm {
			st := p.cfg.CreditHistory.StatsSinceBy(windowStart, func(uid string) bool { return uids[uid] })
			if st.Count == 0 {
				continue
			}
			statsByRealm[realm] = st
			if net := float64(st.Consumed - st.Earned); net > 0 {
				drainByRealm[realm] = net / float64(hours)
			}
		}
	}

	locks := map[string]pool.ModelLockRow{}
	for _, l := range p.cfg.Pool.ModelLockView() {
		locks[l.Realm+"\x1f"+l.Model] = l
	}

	// 并集：用量侧（有消耗/有请求）∪ 锁侧（当前被限流）∪ 目录侧（已知模型）。
	// 两侧都没有的模型既无速率也无限流；目录侧的加入只为让表里能看到「有这个模型，
	// 但窗口内没有请求」，而不是整张表空白（空表会被读成功能坏了）。
	rows := make([]modelEtaRow, 0, len(burns)+len(locks))
	seen := map[string]struct{}{}
	push := func(row modelEtaRow) {
		key := row.Realm + "\x1f" + row.Model
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		rows = append(rows, row)
	}

	// 目录行先铺，后面用量/锁行靠 findRow+replaceRow 覆盖它们。倍率表是纯内存
	// 快照（启动预热与模型页刷新时填充），不打上游；为空时这里什么也不做，
	// 行为与改动前一致。
	if p.cfg.Upstream != nil {
		for realm := range poolByRealm {
			seedCatalogRows(&rows, seen, realm, p.cfg.Upstream.RatesSnapshot(realm), poolByRealm[realm])
		}
	}

	for _, b := range burns {
		realm := b.Realm
		if realm == "" {
			realm = "cn"
		}
		row := modelEtaRow{
			Model:         b.Model,
			Realm:         realm,
			BurnPerHour:   b.CreditsPerHour,
			Credits:       b.Credits,
			CreditSamples: b.CreditSamples,
			Requests:      b.Requests,
			PoolCredits:   poolByRealm[realm],
		}
		switch {
		case b.Requests == 0:
			row.State = "idle"
		case row.PoolCredits <= 0:
			// 余额为 0 时任何估算都没有意义，优先于「模型级没观测到扣费」。
			row.State = "empty"
		case b.Credits > 0 && b.CreditSamples >= etaMinSamples && b.CreditSamples*2 >= b.Requests-b.Errors:
			// 模型级口径可用：有非零积分观测，且过半成功请求都带观测。
			// 分母用成功数而非总数：失败尝试天然没有 usage，把它们算进来会让一个
			// 正常模型的样本占比看起来长期不合格。
			row.State = "ok"
			h, capped := etaHours(row.PoolCredits, b.CreditsPerHour, now, bucketsByRealm[realm])
			row.EtaHours = h
			row.ExpiryCapped = capped
			if h > 0 {
				row.EtaUntil = now.Add(time.Duration(h * float64(time.Hour)))
			}
		default:
			// 模型级观测不可用（上游不回 credit，或回的样本太少），降级到余额口径。
			if !poolFallback(&row, drainByRealm[realm], statsByRealm[realm], now, bucketsByRealm[realm]) {
				switch {
				case b.Credits > 0:
					// 确实扣了积分，只是样本不够：给不出可信速率，别硬编。
					row.State = "insufficient"
				case statsByRealm[realm].Count > 0:
					// 余额有变动但净增（签到/任务补的比花的多）：不会耗尽，「免费中」
					// 也不对——明明在花钱，只是赚得更快。给「余额净增」并带上两侧数字。
					row.State = "growing"
					row.PoolConsumed = statsByRealm[realm].Consumed
					row.PoolEarned = statsByRealm[realm].Earned
				default:
					// 窗口内余额没有任何变动：确实没扣费。
					row.State = "free"
				}
			}
		}
		// 目录行可能已经占了这个 key：用量事实要覆盖掉那行 idle，但倍率只有
		// 目录知道，得从旧行搬过来（用量账本不记倍率）。
		if old, ok := findRow(rows, realm, b.Model); ok {
			row.CreditRate = old.CreditRate
			replaceRow(rows, row)
		} else {
			push(row)
		}
	}

	for key, l := range locks {
		sep := strings.IndexByte(key, '\x1f')
		realm, model := key[:sep], key[sep+1:]
		row, ok := findRow(rows, realm, model)
		if !ok {
			// 只有锁、没有用量：状态按 idle 起步（窗口内确实没请求）。不能用 ok——
			// 那会让前端把「没有速率」渲染成「续航 0」。
			row = modelEtaRow{Model: model, Realm: realm, PoolCredits: poolByRealm[realm], State: "idle"}
		}
		// 限流状态覆盖续航状态：模型整池不可用时，「还能撑 12 天」是误导——
		// 此刻根本用不了。倒计时字段仍一并给出，供前端展示恢复时刻；同时把
		// eta_* 清掉，避免接口消费方（脚本/第三方看板）读到自相矛盾的数。
		switch l.State {
		case "locked":
			row.State = "locked"
			row.EtaHours, row.EtaUntil, row.ExpiryCapped = 0, time.Time{}, false
		case "starved":
			row.State = "starved"
			row.EtaHours, row.EtaUntil, row.ExpiryCapped = 0, time.Time{}, false
		}
		row.Locked = l.Locked
		row.Servable = l.Servable
		row.Total = l.Total
		row.UnlockAt = l.UnlockAt
		row.FullyUnlockAt = l.FullyUnlockAt
		row.Reason = l.Reason
		if ok {
			replaceRow(rows, row)
		} else {
			push(row)
		}
	}

	// 排序：先用不了（locked/starved）→ 快用完（剩余时间升序）→ 其余。
	rank := func(s string) int {
		switch s {
		case "locked":
			return 0
		case "starved":
			return 1
		case "ok":
			return 2
		case "pool":
			return 3
		case "growing":
			return 4
		case "empty":
			return 5
		case "insufficient":
			return 6
		default: // free / idle
			return 7
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		ri, rj := rank(rows[i].State), rank(rows[j].State)
		if ri != rj {
			return ri < rj
		}
		if (rows[i].State == "ok" || rows[i].State == "pool") && rows[i].EtaHours != rows[j].EtaHours {
			return rows[i].EtaHours < rows[j].EtaHours
		}
		if rows[i].Model != rows[j].Model {
			return rows[i].Model < rows[j].Model
		}
		return rows[i].Realm < rows[j].Realm
	})

	// 全池汇总：各域余额 / 消耗 / 速率相加，给出一条「整体还能撑多久」。
	// 模型行在余额口径下本来就是这个数，这里只是把它提到顶层，供表头一行说明。
	var totalPool int64
	var totalBurn float64
	var totalConsumed, totalEarned int64
	for realm, credits := range poolByRealm {
		totalPool += credits
		totalBurn += drainByRealm[realm]
		totalConsumed += statsByRealm[realm].Consumed
		totalEarned += statsByRealm[realm].Earned
	}
	balance := map[string]any{
		"pool_credits":    totalPool,
		"burn_per_hour":   totalBurn,
		"consumed":        totalConsumed,
		"earned":          totalEarned,
		"window_hours":    hours,
		"has_observation": totalConsumed > 0 || totalEarned > 0,
	}
	if totalBurn > 0 && totalPool > 0 {
		h, capped := etaHours(totalPool, totalBurn, now, nil)
		balance["eta_hours"] = h
		balance["expiry_capped"] = capped
		balance["eta_until"] = now.Add(time.Duration(h * float64(time.Hour))).Format(time.RFC3339)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"rows": rows,
		// window_hours 是请求口径，effective_hours 是速率分母实际覆盖的时长
		// （窗口起点对齐整点后的真实跨度）。两者一并回显，前端标注「近 N 小时」
		// 时才不会与数字自相矛盾。
		"window_hours":    hours,
		"effective_hours": effHours,
		"realm_credits":   poolByRealm,
		"balance":         balance,
		"generated":       now.Format(time.RFC3339),
	})
}

// seedCatalogRows 把倍率目录里的模型铺成 idle 行（该域余额为 0 时铺 empty）。
//
// 为什么需要：rows 的来源本来是「窗口内有用量 ∪ 当前被限流」。一个刚重启、或
// 者最近没用过的实例，两者都为空，接口就返回 rows=[]，面板上是整张空表——用户
// 看到的不是「还没请求」，而是「这功能没有数据」。目录是本地已有快照，铺行的
// 成本为零，顺带还能把倍率带出来。
//
// 只铺 idle：目录只知道「有这个模型」，不知道它花不花钱。给它编个续航数字等于
// 凭空造数据；「无请求」才是这一刻唯一诚实的说法。
func seedCatalogRows(rows *[]modelEtaRow, seen map[string]struct{}, realm string, catalog map[string]string, poolCredits int64) {
	if len(catalog) == 0 {
		return
	}
	state := "idle"
	if poolCredits <= 0 {
		// 没余额的域，任何模型都用不了——这才是该显示的状态，别拿「无请求」糊过去。
		state = "empty"
	}
	for model, rate := range catalog {
		key := realm + "\x1f" + model
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		*rows = append(*rows, modelEtaRow{
			Model:       model,
			Realm:       realm,
			State:       state,
			PoolCredits: poolCredits,
			CreditRate:  rate,
		})
	}
}

// poolFallback 用账号余额变动流水给出一行的参考续航。返回 false 表示没有可用
// 的观测（近期没扣过费），调用方回落到 free / insufficient 文案。
//
// 为什么不在这里回落到「∞」或「免费」：余额在降就是有消耗，只是上游没把消耗
// 归到模型头上。给一个标注了来源的近似值，比给一句错的「免费中」有用。
func poolFallback(row *modelEtaRow, burn float64, st credithist.Stats, now time.Time, buckets []expiryPoint) bool {
	if burn <= 0 || row.PoolCredits <= 0 {
		return false
	}
	h, capped := etaHours(row.PoolCredits, burn, now, buckets)
	if h <= 0 {
		return false
	}
	row.State = "pool"
	row.EtaSource = "balance"
	row.EtaHours = h
	row.ExpiryCapped = capped
	row.EtaUntil = now.Add(time.Duration(h * float64(time.Hour)))
	row.PoolBurn = burn
	row.PoolConsumed = st.Consumed
	row.PoolEarned = st.Earned
	return true
}

func findRow(rows []modelEtaRow, realm, model string) (modelEtaRow, bool) {
	for _, r := range rows {
		if r.Realm == realm && r.Model == model {
			return r, true
		}
	}
	return modelEtaRow{}, false
}

func replaceRow(rows []modelEtaRow, row modelEtaRow) {
	for i := range rows {
		if rows[i].Realm == row.Realm && rows[i].Model == row.Model {
			rows[i] = row
			return
		}
	}
}
