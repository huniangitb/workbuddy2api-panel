// credit-forecast —— 本地 CLI：把「模型每日配额 × 域内账号数」与「今日实际用量」
// 对比，输出各模型的限流临近度，供自动化调用方（agent）分派决策。
//
// 不改网关：全部数据来自两个现有只读接口——
//
//	GET /v1/credit_status   模型倍率 / 限流画像 / 各域账号明细（本机网关自带）
//	GET /panel/api/usage    按模型聚合的今日 token 消耗（from/to=CST 自然日界）
//
// 配额口径（上游不暴露剩余额度，按实测规则本地推算）：单账号每日 token 上限，
// 池级配额 = 配额 × 域内账号数；占用比 = 今日该域该模型总 token ÷ 池级配额。
// 缺省内置两条实测规则（CN 200M、国际 100M），--quota 可覆盖/追加。
//
// 用法：
//
//	go run ./cmd/credit-forecast            # 人读表格
//	go run ./cmd/credit-forecast --json     # 机器读（agent 用）
//	go run ./cmd/credit-forecast --addr 127.0.0.1:7863 --key <api_key>
//	go run ./cmd/credit-forecast --quota cn:deepseek-v4.1-flash=200000000
//
// 默认从工作目录 config.json 读 api_key 与 listen，零配置可用（本地环境直连）。
package main

import (
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// cst 本地自然日口径固定 UTC+8（与网关积分闸/账本的 CST 自然日一致，
// 不随宿主机时区漂移）。
var cst = time.FixedZone("CST", 8*60*60)

// defaultQuotas 单账号每日 token 配额实测规则。键 = "域:模型"。
var defaultQuotas = map[string]int64{
	"cn:deepseek-v4.1-flash":     200_000_000,
	"global:deepseek-v4.1-flash": 100_000_000,
}

type creditStatus struct {
	Realms map[string]struct {
		TotalCredits int64 `json:"total_credits"`
		Accounts     []struct {
			UID      string `json:"uid"`
			Credits  int64  `json:"credits"`
			Cooling  bool   `json:"cooling"`
			Disabled bool   `json:"disabled"`
		} `json:"accounts"`
	} `json:"realms"`
	Models []struct {
		Model      string   `json:"model"`
		Realm      string   `json:"realm"`
		Rate       string   `json:"rate"`
		RateValue  *float64 `json:"rate_value"`
		LimitState string   `json:"limit_state"`
		Locked     *int     `json:"locked"`
		Servable   *int     `json:"servable"`
		Total      *int     `json:"total"`
		UnlockAt   string   `json:"unlock_at"`
	} `json:"models"`
}

type usageSnapshot struct {
	// ByModel 跨域合并的裸模型行（realm 缺失），只做无 credit 样本时的回退。
	ByModel []struct {
		Key         string `json:"key"`
		TotalTokens int64  `json:"total_tokens"`
	} `json:"by_model"`
	// CreditByModel 按「域:模型」分组的回执行（键与 /v1/models 调用值同形）。
	// CreditTokens 是该 (域, 模型) 拿到 credit 的请求 token 合计——上游末帧几乎
	// 必带 credit（实测 2793 个请求 2790 个有），是按域拆分用量的可靠来源。
	CreditByModel []struct {
		Key          string  `json:"key"`
		Realm        string  `json:"realm"`
		CreditTokens int64   `json:"credit_tokens"`
		Credits      float64 `json:"credits"`
	} `json:"credit_by_model"`
}

func main() {
	cfgPath := flag.String("config", "config.json", "网关 config.json 路径（自动读 api_key/listen）")
	addr := flag.String("addr", "", "网关地址 host:port（默认取 config.json 的 listen）")
	key := flag.String("key", "", "Bearer api_key（默认取 config.json 的 api_key）")
	jsonOut := flag.Bool("json", false, "输出 JSON（供 agent 消费）")
	realmFilter := flag.String("realm", "", "只看某域（cn|global），空 = 全部")
	insecure := flag.Bool("insecure", false, "跳过 TLS 校验（https 自签时用）")
	showAll := flag.Bool("all", false, "显示全部模型（默认只显示配额规则内的模型）")
	flag.Parse()

	quotas := map[string]int64{}
	for k, v := range defaultQuotas {
		quotas[k] = v
	}
	// flag 包不支持同名 flag 重复，-quota 的「可多次」语义从 os.Args 手工摘取。
	for i := 0; i < len(os.Args)-1; i++ {
		a := os.Args[i]
		if a != "-quota" && a != "--quota" {
			continue
		}
		kv := strings.SplitN(os.Args[i+1], "=", 2)
		if len(kv) != 2 {
			fatal("--quota 需要键=值形式: %q", os.Args[i+1])
		}
		n, err := strconv.ParseInt(kv[1], 10, 64)
		if err != nil || n < 0 {
			fatal("--quota 值须为非负整数: %s=%s", kv[0], kv[1])
		}
		quotas[strings.TrimSpace(kv[0])] = n
	}

	// 配置：config.json 给 listen/api_key 缺省，命令行显式传参优先。
	// 查找顺序：显式 --config（默认 ./config.json，即网关工作目录内调用零配置）→
	// WB2API_CONFIG 环境变量——其余场景请显式传 --addr/--key。
	listen, apiKey := "", ""
	configCandidates := []string{*cfgPath}
	if env := os.Getenv("WB2API_CONFIG"); env != "" {
		configCandidates = append(configCandidates, env)
	}
	for _, p := range configCandidates {
		if raw, err := os.ReadFile(p); err == nil {
			var cfg struct {
				APIKey string `json:"api_key"`
				Listen string `json:"listen"`
			}
			if json.Unmarshal(raw, &cfg) == nil {
				listen, apiKey = cfg.Listen, cfg.APIKey
			}
			break
		}
	}
	if *addr != "" {
		listen = *addr
	}
	if *key != "" {
		apiKey = *key
	}
	listen = strings.TrimSpace(listen)
	listen = strings.TrimPrefix(listen, "0.0.0.0:")
	// listen 常见形态 ":7863"（仅端口）：本地环境默认回环。
	if strings.HasPrefix(listen, ":") {
		listen = "127.0.0.1" + listen
	}
	if listen == "" {
		fatal("网关地址为空：config.json 缺 listen 且未传 --addr")
	}
	base := "http://" + listen
	tr := &http.Transport{}
	if *insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	client := &http.Client{Timeout: 15 * time.Second, Transport: tr}

	// CST 自然日界：今日 00:00 → 现在。
	now := time.Now().In(cst)
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, cst)

	var cs creditStatus
	getJSON(client, base+"/v1/credit_status", apiKey, &cs)
	var us usageSnapshot
	getJSON(client, fmt.Sprintf("%s/panel/api/usage?from=%d&to=%d",
		base, dayStart.Unix(), now.Unix()), apiKey, &us)

	// 今日 (域, 模型) 用量表：credit 回执行按域拆分（键=域:模型，覆盖近乎全部
	// 请求）；某模型当日没有任何 credit 样本时回退跨域合并的 by_model 总量
	//（此时域无法区分，两域同配额规则时量级仍可用，结果带近似性）。
	used := map[string]int64{}     // key = realm + "\x1f" + 裸模型名（credit 来源，精确）
	bareUsed := map[string]int64{} // key = 裸模型名（by_model 回退）
	hasCreditRow := map[string]bool{}
	for _, m := range us.CreditByModel {
		bare := m.Key
		if i := strings.Index(bare, ":"); i >= 0 {
			bare = bare[i+1:]
		}
		used[m.Realm+"\x1f"+bare] = m.CreditTokens
		hasCreditRow[m.Realm+"\x1f"+bare] = true
	}
	for _, m := range us.ByModel {
		bareUsed[m.Key] = m.TotalTokens
	}

	// 域内账号数：配额按账号数放大；禁用号不消耗配额，不计入分母。
	acctCount := map[string]int{}
	for realm, r := range cs.Realms {
		for _, a := range r.Accounts {
			if !a.Disabled {
				acctCount[realm]++
			}
		}
	}

	// 今日首次调用时刻（每配额模型）：恢复点 = 首调 + 24h（上游配额窗口按天计，
	// 从当日首次请求起算）。request_logs 只回「最近 N 条」，当日请求超一页时用
	// To 游标向前翻页直到翻到最早一条；翻页次数封顶防失控（超限时为首调上限，
	// 真实首调更早，恢复时间只会更晚——估计偏保守方向）。
	firstCall := map[string]time.Time{} // key = 完整模型名（cn:x / global:x）
	for _, m := range cs.Models {
		bare := m.Model
		if i := strings.Index(bare, ":"); i >= 0 {
			bare = bare[i+1:]
		}
		if _, hasRule := quotas[m.Realm+":"+bare]; !hasRule {
			continue
		}
		fc, truncated := fetchFirstCall(client, base, apiKey, m.Model, dayStart)
		if !fc.IsZero() {
			firstCall[m.Model] = fc
		}
		if truncated {
			fmt.Fprintf(os.Stderr, "credit-forecast: %s 今日首调翻页封顶，恢复时间为下界估计\n", m.Model)
		}
	}

	type row struct {
		Model      string   `json:"model"`
		Realm      string   `json:"realm,omitempty"`
		RateValue  *float64 `json:"rate_value,omitempty"`
		LimitState string   `json:"limit_state"`
		UsedToday  int64    `json:"used_today_tokens"`
		PoolQuota  int64    `json:"pool_quota_tokens"`
		UsedRatio  *float64 `json:"used_ratio,omitempty"`
		Status     string   `json:"status"`
		// 恢复口径二选一：已触发模型锁 → unlock_at（网关限流给出的上游恢复时刻，
		// 权威）；未触发 → 当日首次调用 + 24h（配额窗口按天推算）。
		RecoversAt    string `json:"recovers_at,omitempty"`
		RecoverSource string `json:"recover_source,omitempty"` // "gateway-lock" | "quota-window"
		RecoverInMin  int    `json:"recover_in_minutes,omitempty"`
		FirstCall     string `json:"first_call_cst,omitempty"`
	}
	var rows []row
	for _, m := range cs.Models {
		// 默认只显示配额规则内的模型——本 CLI 的职责是限流临近度，没配额规则
		// 的模型没有「快到额度」可言；--all 才展开全量倍率/限流清单。
		bare := m.Model
		if i := strings.Index(bare, ":"); i >= 0 {
			bare = bare[i+1:]
		}
		if !*showAll {
			if _, hasRule := quotas[m.Realm+":"+bare]; !hasRule {
				continue
			}
		}
		if *realmFilter != "" && m.Realm != *realmFilter {
			continue
		}
		qkey := m.Realm + ":" + bare
		q, hasQ := quotas[qkey]
		r := row{
			Model: m.Model, Realm: m.Realm, RateValue: m.RateValue,
			LimitState: m.LimitState,
		}
		// 用量：credit 回执行优先（按域精确）；无样本时回退跨域合并总量。
		if hasCreditRow[m.Realm+"\x1f"+bare] {
			r.UsedToday = used[m.Realm+"\x1f"+bare]
		} else {
			r.UsedToday = bareUsed[bare]
		}
		if hasQ {
			r.PoolQuota = q * int64(acctCount[m.Realm])
			if r.PoolQuota > 0 {
				ratio := float64(r.UsedToday) / float64(r.PoolQuota)
				r.UsedRatio = &ratio
				r.Status = grade(ratio, m.LimitState)
			}
		} else {
			r.Status = "no-quota-rule"
		}
		if m.LimitState != "ok" && m.LimitState != "" && hasQ {
			r.Status = m.LimitState // 已触发模型锁：状态让位于真实锁态
		}
		// 恢复口径：网关锁态给出的 unlock_at 权威（6004 冷却的上游恢复时刻）；
		// 未触发锁时按「当日首次调用 + 24h」推算配额窗口。字段统一 RFC3339
		//（含日期）——首调在昨天深夜时恢复点落在今天、反之亦然，只给时刻会有歧义。
		if fc, ok := firstCall[m.Model]; ok {
			r.FirstCall = fc.In(cst).Format(time.RFC3339)
		}
		switch {
		case m.UnlockAt != "" && (m.LimitState == "locked" || m.LimitState == "starved"):
			r.RecoversAt = m.UnlockAt
			r.RecoverSource = "gateway-lock"
		case !firstCall[m.Model].IsZero():
			r.RecoversAt = firstCall[m.Model].Add(24 * time.Hour).Format(time.RFC3339)
			r.RecoverSource = "quota-window"
		}
		if t, err := time.Parse(time.RFC3339, r.RecoversAt); err == nil && t.After(now) {
			r.RecoverInMin = int(time.Until(t).Minutes())
		}
		rows = append(rows, r)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		// 排序：先按危险度（over/near/warn 在前），同级按占用比降序，无比按倍率升序。
		rank := func(s string) int {
			switch s {
			case "over", "locked":
				return 0
			case "near":
				return 1
			case "warn":
				return 2
			case "starved":
				return 3
			}
			return 4
		}
		ri, rj := rank(rows[i].Status), rank(rows[j].Status)
		if ri != rj {
			return ri < rj
		}
		fi, fj := rows[i].UsedRatio != nil, rows[j].UsedRatio != nil
		if fi != fj {
			return fi
		}
		if fi && *rows[i].UsedRatio != *rows[j].UsedRatio {
			return *rows[i].UsedRatio > *rows[j].UsedRatio
		}
		fvi, fvj := rows[i].RateValue != nil, rows[j].RateValue != nil
		if fvi != fvj {
			return fvi
		}
		if fvi {
			return *rows[i].RateValue < *rows[j].RateValue
		}
		return rows[i].Model < rows[j].Model
	})

	if *jsonOut {
		// agent 视角的最小集：分派判断只需要 models[]；域配额汇总（按域选号）
		// 保留 total_credits 一层，账号明细省略——要逐号诊断走面板接口。
		realmTotals := map[string]int64{}
		for realm, r := range cs.Realms {
			realmTotals[realm] = r.TotalCredits
		}
		out, _ := json.MarshalIndent(map[string]any{
			"day_cst":  now.Format("2006-01-02"),
			"models":   rows,
			"credits":  realmTotals,
			"accounts": acctCount,
		}, "", "  ")
		fmt.Println(string(out))
		return
	}

	// 人读表格：精简到决策必需列；恢复时刻只给本地时钟 HH:MM。
	w := []int{34, 8, 9, 10, 6, 8, 9}
	h := fmt.Sprintf("%-*s %-*s %-*s %-*s %-*s %-*s %-*s",
		w[0], "model", w[1], "limit", w[2], "used_today",
		w[3], "pool_quota", w[4], "ratio", w[5], "recovers", w[6], "status")
	fmt.Println(h)
	fmt.Println(strings.Repeat("-", len(h)))
	for _, r := range rows {
		ratio := "-"
		if r.UsedRatio != nil {
			ratio = fmt.Sprintf("%.0f%%", *r.UsedRatio*100)
		}
		recovers := "-"
		if r.RecoversAt != "" {
			if t, err := time.Parse(time.RFC3339Nano, r.RecoversAt); err == nil {
				recovers = t.In(cst).Format("15:04") + fmt.Sprintf("(%dh%02dm)", int(time.Until(t).Hours()), int(time.Until(t).Minutes())%60)
			} else {
				recovers = r.RecoversAt
			}
		}
		fmt.Printf("%-*s %-*s %-*s %-*s %-*s %-*s %-*s\n",
			w[0], r.Model, w[1], r.LimitState,
			w[2], human(r.UsedToday), w[3], human(r.PoolQuota),
			w[4], ratio, w[5], recovers, w[6], r.Status)
	}
}

// fetchFirstCall 从 request_logs 找该模型今日最早一条成功请求。接口按「最近
// N 条」返回且单页上限 1000：当日请求超一页时以最旧一条的时间为 To 游标继续
// 向前翻，直到翻尽或封顶。返回 (首调时刻, 是否因封顶而只是下界)。
func fetchFirstCall(c *http.Client, base, key, fullModel string, dayStart time.Time) (time.Time, bool) {
	bare := fullModel
	if i := strings.Index(bare, ":"); i >= 0 {
		bare = bare[i+1:]
	}
	var earliest time.Time
	to := time.Now()
	for page := 0; page < 10; page++ {
		var resp struct {
			Entries []struct {
				Time  string `json:"time"`
				Model string `json:"model"`
				OK    bool   `json:"ok"`
			} `json:"entries"`
		}
		url := fmt.Sprintf("%s/panel/api/request_logs?model=%s&from=%d&to=%d&limit=1000",
			base, bare, dayStart.Unix(), to.Unix())
		getJSON(c, url, key, &resp)
		if len(resp.Entries) == 0 {
			break
		}
		var oldestFull time.Time // 本页最旧一条（含不匹配行，作下一页游标）
		for _, e := range resp.Entries {
			t, err := time.Parse(time.RFC3339Nano, e.Time)
			if err != nil {
				continue
			}
			if oldestFull.IsZero() || t.Before(oldestFull) {
				oldestFull = t
			}
			if !e.OK || e.Model != fullModel {
				continue // 配额窗口按真实消耗计：只认成功请求
			}
			if earliest.IsZero() || t.Before(earliest) {
				earliest = t
			}
		}
		if len(resp.Entries) < 1000 {
			return earliest, false // 本页未满 = 已翻到当日最早，估计精确
		}
		if !oldestFull.IsZero() && !oldestFull.Before(dayStart) {
			to = oldestFull // 游标前移，继续翻
			continue
		}
		break
	}
	return earliest, true
}

// grade 占用比分档：<80% ok，≥80% warn，≥95% near，≥100% over。
func grade(ratio float64, _ string) string {
	switch {
	case ratio >= 1:
		return "over"
	case ratio >= 0.95:
		return "near"
	case ratio >= 0.8:
		return "warn"
	}
	return "ok"
}

// human token 数缩写：12.3M / 850K / 999。
func human(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.0fK", float64(n)/1_000)
	}
	return strconv.FormatInt(n, 10)
}

func getJSON(c *http.Client, url, key string, out any) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		fatal("请求构造失败: %v", err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := c.Do(req)
	if err != nil {
		fatal("请求 %s 失败: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		fatal("%s -> HTTP %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		fatal("%s 响应解析失败: %v", url, err)
	}
}

func fatal(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "credit-forecast: "+f+"\n", a...)
	os.Exit(1)
}
