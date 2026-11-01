// credit_status_test.go GET /v1/credit_status：积分汇总、倍率排序、限流画像合并。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// TestRealmCreditSummary 按域汇总：只累计本域账号（冷却/禁用号的钱照算），
// 未启用的域不出现在结果里。
func TestRealmCreditSummary(t *testing.T) {
	list := []pool.Status{
		{UID: "c1", Realm: "cn", Credits: 100},
		{UID: "c2", Realm: "cn", Credits: 50, Cooling: true},
		{UID: "g1", Realm: "global", Credits: 7},
		{UID: "x1", Realm: "cn", Credits: 999, Disabled: true},
	}
	got := realmCreditSummary(list, map[string]bool{"cn": true})
	cn, ok := got["cn"].(map[string]any)
	if !ok {
		t.Fatalf("cn 域缺失: %v", got)
	}
	if cn["total_credits"] != int64(1149) {
		t.Fatalf("cn total=%v want 1149（冷却/禁用号照算）", cn["total_credits"])
	}
	if accts, _ := cn["accounts"].([]map[string]any); len(accts) != 3 {
		t.Fatalf("cn accounts=%d want 3", len(accts))
	}
	if _, ok := got["global"]; ok {
		t.Fatalf("未启用的域不应出现: %v", got)
	}
}

// TestCreditStatusModelsSortAndLimit 倍率升序（无法数值化的排末尾）；
// 有锁行的模型带 limit_state 等画像，无锁行标 ok。
func TestCreditStatusModelsSortAndLimit(t *testing.T) {
	rates := map[string]map[string]string{
		"cn": {"glm-a": "0.5", "glm-b": "0.05", "glm-c": "按量", "glm-d": "0.05"},
	}
	locks := []pool.ModelLockRow{{
		Model: "glm-b", Realm: "cn", State: "locked", Locked: 2, Servable: 0, Total: 3,
	}}
	got := creditStatusModels(rates, locks)
	if len(got) != 4 {
		t.Fatalf("条目数=%d want 4", len(got))
	}
	wantOrder := []string{"cn:glm-b", "cn:glm-d", "cn:glm-a", "cn:glm-c"}
	for i, want := range wantOrder {
		if got[i]["model"] != want {
			t.Fatalf("排序[%d]=%v want %s", i, got[i]["model"], want)
		}
	}
	locked := got[0]
	if locked["limit_state"] != "locked" || locked["locked"] != 2 || locked["total"] != 3 {
		t.Fatalf("锁画像不符: %v", locked)
	}
	if v, ok := locked["rate_value"].(float64); !ok || v != 0.05 {
		t.Fatalf("锁定条目倍率可解析时应带 rate_value: %v", locked)
	}
	if got[3]["limit_state"] != "ok" {
		t.Fatalf("无锁行应标 ok: %v", got[3])
	}
	if _, ok := got[3]["rate_value"]; ok {
		t.Fatalf("不可数值化倍率不应伪造 rate_value: %v", got[3])
	}
}

// TestCreditStatusEndpoint 端到端：路由可达、Bearer 鉴权、响应形状成立。
func TestCreditStatusEndpoint(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
	)
	p.SetCredits("c1", 88, 0)
	// APIKey 非空才启用 Bearer 鉴权（空 key 放行一切）。
	h := NewHandler(Config{Pool: p, Upstream: &upstream.Client{}, APIKey: "k"})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/credit_status", nil)
	req.Header.Set("Authorization", "Bearer k")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Realms map[string]struct {
			TotalCredits int64            `json:"total_credits"`
			Accounts     []map[string]any `json:"accounts"`
		} `json:"realms"`
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON: %v (%s)", err, rec.Body.String())
	}
	cn, ok := out.Realms["cn"]
	if !ok || cn.TotalCredits != 88 {
		t.Fatalf("cn 汇总不符: %v", out.Realms)
	}
	if len(cn.Accounts) != 1 || cn.Accounts[0]["uid"] != "c1" {
		t.Fatalf("cn 账号明细不符: %v", cn.Accounts)
	}
	if out.Models != nil && len(out.Models) != 0 {
		t.Fatalf("目录未刷新时倍率表应为空: %v", out.Models)
	}

	// 鉴权：错误 key 拒绝。
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/v1/credit_status", nil)
	req2.Header.Set("Authorization", "Bearer wrong")
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized || !strings.Contains(rec2.Body.String(), "invalid_api_key") {
		t.Fatalf("错误 key 应 401: %d %s", rec2.Code, rec2.Body.String())
	}
}
