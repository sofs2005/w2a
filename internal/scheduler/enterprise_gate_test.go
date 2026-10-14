package scheduler

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// TestEnterpriseAccountsSkipGrowthTasks 企业版账号在 growth 系循环（签到 / 旅行 /
// 连登奖励）中**零上游调用**。
//
// 上游实测（fork linguo2625469 7e3bc51，2026-10-07 同一时刻 A/B 对照）：企业版无
// 个人成长体系——daily-checkin 与 claim-gift 返 400 code 10001「企业账号不支持该
// 操作」，/activity/growth/* 返 403「growth system is only available for personal
// users」。不门控则每天对这四类端点空发注定失败的调用并刷 WARN。
//
// 企业版与 global 正交：本例用 **CN realm + enterpriseId** 的账号，专门证明
// 「CN 账号一律放行」不足以覆盖——只有 IsEnterprise() 门控生效才能让 calls==0。
func TestEnterpriseAccountsSkipGrowthTasks(t *testing.T) {
	fastTravel(t)
	fastActivity(t)

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "no upstream call expected for enterprise", 404)
	}))
	defer srv.Close()

	p := pool.New("")
	// CN 账号（无 Domain → realm=cn）但带 enterpriseId：global 门控拦不到它。
	p.Add(&auth.Auth{UID: "e1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999,
		EnterpriseID: "ent-1"})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	s.CheckinAll()
	s.RunTravelNow()
	s.claimGrowthRewards(p.AuthByUID("e1"))

	if n := calls.Load(); n != 0 {
		t.Errorf("enterprise account growth upstream calls=%d want 0（签到/旅行/连登奖励均应跳过）", n)
	}
}

// TestEnterpriseAccountSkippedListedWithStatus 门控跳过的企业版账号在 CheckinAll
// 回执里以 skipped(enterprise) 呈现——手动触发时结果可读，与 global 的
// skipped(global) 区分开（两种跳过原因不可混淆，排查时判据不同）。
func TestEnterpriseAccountSkippedListedWithStatus(t *testing.T) {
	fastTravel(t)
	fastActivity(t)

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "no upstream call expected for enterprise", 404)
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "e1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999,
		EnterpriseID: "ent-1"})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	out, err := s.CheckinAll()
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(out) != 1 || out[0].UID != "e1" {
		t.Fatalf("out=%+v want 1 row for e1", out)
	}
	if out[0].Status != CheckinSkipped {
		t.Errorf("status=%q want skipped（企业版门控回执）", out[0].Status)
	}
	if out[0].Detail != "enterprise" {
		t.Errorf("detail=%q want enterprise", out[0].Detail)
	}
	if calls.Load() != 0 {
		t.Errorf("upstream calls=%d want 0", calls.Load())
	}
}

// TestEnterpriseGateDoesNotBlockNonEnterprise 反向对照：同为 CN realm 但**不带**
// enterpriseId 的账号照常跑签到（证明门控判的是 enterpriseId，不是「CN 账号一律跳过」
// 这类过宽条件）。若无此例，把门控误写成 `if a.IsEnterprise() || true` 也测不出来。
func TestEnterpriseGateDoesNotBlockNonEnterprise(t *testing.T) {
	fastTravel(t)
	fastActivity(t)

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(`{"code":0,"msg":"OK"}`))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "cn1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	out, err := s.CheckinAll()
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if calls.Load() == 0 {
		t.Errorf("non-enterprise CN account made 0 upstream calls: out=%+v（门控过宽，误伤了普通账号）", out)
	}
}

// TestEnterpriseAccountStillKeepsAlive keepalive 不拦企业版：token refresh 与个人
// 成长体系无关，企业版账号照样要刷新（与 global 不拦 keepalive 同理）。
func TestEnterpriseAccountStillKeepsAlive(t *testing.T) {
	f := &fakeUpstream{}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "e1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: 1,
		EnterpriseID: "ent-1"}
	p.Add(a)

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})
	s.RunKeepaliveNow()
	if f.refreshCalls.Load() != 1 {
		t.Errorf("enterprise keepalive refresh calls=%d want 1（keepalive 不拦企业版）", f.refreshCalls.Load())
	}
	if a.AccessToken != "new" {
		t.Errorf("enterprise token 未刷新: %s", a.AccessToken)
	}
}
