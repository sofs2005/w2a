package upstream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"workbuddy2api/internal/auth"
)

// TestBuddyQuotaQuery 解析 quota 响应（实测字段：affordable/balance/cost_per_open/max_open_count）。
func TestBuddyQuotaQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/activity/growth/buddy/quota" {
			http.Error(w, "not found", 404)
			return
		}
		w.Write([]byte(`{"code":0,"msg":"OK","data":{"affordable":1,"balance":18,"cost_per_open":10,"max_open_count":5}}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	q, err := c.BuddyQuotaQuery(&auth.Auth{UID: "u1", AccessToken: "at"})
	if err != nil {
		t.Fatalf("BuddyQuotaQuery: %v", err)
	}
	if q.Affordable != 1 || q.Balance != 18 || q.CostPerOpen != 10 || q.MaxOpenCount != 5 {
		t.Errorf("quota=%+v want affordable=1 balance=18 cost=10 max=5", q)
	}
}

// TestBuddyOpen SendsCountAndParsesResults 发 {"count":N} 并解析开出结果。
func TestBuddyOpenSendsCountAndParsesResults(t *testing.T) {
	var gotCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/activity/growth/buddy/open" {
			http.Error(w, "not found", 404)
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("method=%s want POST", r.Method)
		}
		var body struct {
			Count int `json:"count"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		gotCount = body.Count
		w.Write([]byte(`{"code":0,"msg":"OK","data":{"count":1,"results":[` +
			`{"instance":{"id":25053427,"template_id":12,"is_shiny":0,"soul_desc":"价值投资官","source":"gacha"},` +
			`"template":{"id":12,"name":"金算喵","description":"价值投资官"}}]}}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	resp, err := c.BuddyOpen(&auth.Auth{UID: "u1", AccessToken: "at"}, 1)
	if err != nil {
		t.Fatalf("BuddyOpen: %v", err)
	}
	if gotCount != 1 {
		t.Errorf("请求 count=%d want 1", gotCount)
	}
	if resp.Count != 1 || len(resp.Results) != 1 {
		t.Fatalf("resp=%+v want count=1 results=1", resp)
	}
	r0 := resp.Results[0]
	if r0.Template.Name != "金算喵" || r0.Template.ID != 12 || r0.Instance.Source != "gacha" {
		t.Errorf("result=%+v want 金算喵/template=12/source=gacha", r0)
	}
}

// TestBuddyOpenClampsCount 非法 count 归一到 1（避免把「没想开」误发成扣费请求）。
func TestBuddyOpenClampsCount(t *testing.T) {
	for _, in := range []int{0, -5} {
		var gotCount int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Count int `json:"count"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			gotCount = body.Count
			w.Write([]byte(`{"code":0,"data":{"count":1,"results":[]}}`))
		}))
		c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
		if _, err := c.BuddyOpen(&auth.Auth{UID: "u1", AccessToken: "at"}, in); err != nil {
			t.Fatalf("BuddyOpen(%d): %v", in, err)
		}
		srv.Close()
		if gotCount != 1 {
			t.Errorf("输入 count=%d 时发出 count=%d want 1", in, gotCount)
		}
	}
}

// TestBuddyOpenErrorPropagates 上游错误按 *Error 返回。
func TestBuddyOpenErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`{"code":500,"msg":"system busy"}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	if _, err := c.BuddyOpen(&auth.Auth{UID: "u1", AccessToken: "at"}, 1); err == nil {
		t.Fatal("want error on HTTP 500")
	}
}
