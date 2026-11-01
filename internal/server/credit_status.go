// credit_status.go GET /v1/credit_status：账号积分 × 模型倍率 × 限流状态的只读聚合。
// 供自动化调用方一次拉齐分派决策所需的三类账面信息：各域剩余积分（选域）、
// 模型倍率（选模型，默认升序）、模型限流画像（避开被锁模型）。数据全部来自
// 进程内快照（池状态 / 目录倍率缓存 / 模型锁台账），不打上游——积分是后台
// 刷新的缓存值；倍率表在目录首次成功刷新前为空。只读端点，不改任何状态。
package server

import (
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
)

func (h *Handler) creditStatus(w http.ResponseWriter, r *http.Request) {
	enabled := map[string]bool{"cn": true, "global": h.cfg.Upstream.GlobalEnabled}
	ratesByRealm := make(map[string]map[string]string, 2)
	for realm := range enabled {
		if enabled[realm] {
			ratesByRealm[realm] = h.cfg.Upstream.ModelRatesSnapshot(realm)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"realms": realmCreditSummary(h.cfg.Pool.List(), enabled),
		"models": creditStatusModels(ratesByRealm, h.cfg.Pool.ModelLockView()),
	})
}

// realmCreditSummary 按域汇总剩余积分并附账号明细。积分取池内缓存值（后台
// 余额刷新口径），冷却/禁用号照算——钱还在账上，只是暂时不接单；调用方
// 结合每账号的 cooling/disabled 自行决定口径。
func realmCreditSummary(list []pool.Status, enabled map[string]bool) map[string]any {
	out := map[string]any{}
	for _, realm := range []string{"cn", "global"} {
		if !enabled[realm] {
			continue
		}
		var total int64
		accounts := []map[string]any{}
		for _, st := range list {
			if st.Realm != realm {
				continue
			}
			total += st.Credits
			accounts = append(accounts, map[string]any{
				"uid":      st.UID,
				"credits":  st.Credits,
				"cooling":  st.Cooling,
				"disabled": st.Disabled,
			})
		}
		out[realm] = map[string]any{"total_credits": total, "accounts": accounts}
	}
	return out
}

// creditStatusModels 拼装倍率 × 限流画像，按倍率升序排（无法数值化的倍率
// 排末尾，同为空时按模型名稳定排序）。model 值带域前缀，即聊天调用可直接
// 填写的完整模型名。无锁行的模型 limit_state=ok（锁台账只记有未过期限流的
// (域, 模型)，面板「模型锁池」同口径）。
func creditStatusModels(ratesByRealm map[string]map[string]string, locks []pool.ModelLockRow) []map[string]any {
	lockByKey := map[string]pool.ModelLockRow{}
	for _, l := range locks {
		lockByKey[l.Realm+"\x1f"+l.Model] = l
	}
	type entry struct {
		m        map[string]any
		rateVal  float64
		hasRate  bool
		fullName string
	}
	var out []entry
	for realm, rates := range ratesByRealm {
		for model, rate := range rates {
			full := realm + ":" + model
			e := entry{m: map[string]any{
				"model": full,
				"realm": realm,
				"rate":  rate,
			}, fullName: full}
			if v, err := strconv.ParseFloat(rate, 64); err == nil {
				e.rateVal, e.hasRate = v, true
				e.m["rate_value"] = v
			}
			if l, ok := lockByKey[realm+"\x1f"+model]; ok {
				e.m["limit_state"] = l.State
				e.m["locked"] = l.Locked
				e.m["servable"] = l.Servable
				e.m["total"] = l.Total
				if !l.UnlockAt.IsZero() {
					e.m["unlock_at"] = l.UnlockAt.Format(time.RFC3339)
				}
				if !l.FullyUnlockAt.IsZero() {
					e.m["fully_unlock_at"] = l.FullyUnlockAt.Format(time.RFC3339)
				}
				if l.Reason != "" {
					e.m["limit_reason"] = l.Reason
				}
			} else {
				e.m["limit_state"] = "ok"
			}
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		switch {
		case out[i].hasRate != out[j].hasRate:
			return out[i].hasRate
		case out[i].hasRate && out[i].rateVal != out[j].rateVal:
			return out[i].rateVal < out[j].rateVal
		}
		return out[i].fullName < out[j].fullName
	})
	res := make([]map[string]any, 0, len(out))
	for _, e := range out {
		res = append(res, e.m)
	}
	return res
}
