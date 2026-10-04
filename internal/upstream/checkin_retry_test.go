// checkin_retry_test.go 钉住签到/余额维护类计费调用的瞬时错误有界重试：
// 上游 5xx（实测偶发 code 10000 / http 500）与网络抖动重试，业务错误（已签到/
// 参数错）不重试——重试只会原样再失败一次。
package upstream

import (
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// shortBillingRetry 测试用重试间隔（生产 2s 会让单测秒级膨胀）。
func shortBillingRetry(t *testing.T) {
	t.Helper()
	old := billingRetryDelay
	billingRetryDelay = time.Millisecond
	t.Cleanup(func() { billingRetryDelay = old })
}

func TestDailyCheckinRetriesTransient500(t *testing.T) {
	shortBillingRetry(t)
	var calls int32
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v2/billing/meter/daily-checkin") {
			return nil, errors.New("wrong path")
		}
		if atomic.AddInt32(&calls, 1) == 1 {
			return jsonResp(500, `{"code":10000,"msg":"API request failed with status code: 500"}`), nil
		}
		return jsonResp(200, `{"code":0,"data":{}}`), nil
	})
	if err := c.DailyCheckin(&auth.Auth{AccessToken: "at"}); err != nil {
		t.Fatalf("首次 500 应重试成功，err=%v", err)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("calls=%d want 2（1 次失败 + 1 次重试）", n)
	}
}

func TestDailyCheckinRetriesExhausted(t *testing.T) {
	shortBillingRetry(t)
	var calls int32
	c := testClient(func(r *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return jsonResp(500, `{"code":10000,"msg":"API request failed with status code: 500"}`), nil
	})
	err := c.DailyCheckin(&auth.Auth{AccessToken: "at"})
	if err == nil {
		t.Fatal("持续 500 应返回错误")
	}
	if n := atomic.LoadInt32(&calls); n != 3 {
		t.Fatalf("calls=%d want 3（1 次 + 2 次重试封顶）", n)
	}
}

func TestDailyCheckinNoRetryOnBusinessError(t *testing.T) {
	shortBillingRetry(t)
	var calls int32
	c := testClient(func(r *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		// 「今天已签到」是业务幂等拒绝（code!=0），重试无意义。
		return jsonResp(200, `{"code":14001,"msg":"今日已签到"}`), nil
	})
	err := c.DailyCheckin(&auth.Auth{AccessToken: "at"})
	if err == nil || !IsAlreadyCheckin(err) {
		t.Fatalf("err=%v want already-checkin business error", err)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("calls=%d want 1（业务错误不重试）", n)
	}
}

func TestDailyCheckinNoRetryOn4xx(t *testing.T) {
	shortBillingRetry(t)
	var calls int32
	c := testClient(func(r *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return jsonResp(403, `{"code":11140,"msg":"request illegal"}`), nil
	})
	if err := c.DailyCheckin(&auth.Auth{AccessToken: "at"}); err == nil {
		t.Fatal("403 应返回错误")
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("calls=%d want 1（4xx 非瞬时，不重试）", n)
	}
}

func TestUserResourceExpiryRetriesTransient500(t *testing.T) {
	shortBillingRetry(t)
	var calls int32
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v2/billing/meter/get-user-resource") {
			return nil, errors.New("wrong path")
		}
		if atomic.AddInt32(&calls, 1) == 1 {
			return jsonResp(500, `{"code":10000,"msg":"API request failed with status code: 500"}`), nil
		}
		return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"Accounts":[
			{"PackageName":"p","CapacitySize":100,"CapacityRemain":60}
		]}}}}`), nil
	})
	remain, _, _, err := c.UserResourceExpiry(&auth.Auth{AccessToken: "at"}, 0)
	if err != nil || remain != 60 {
		t.Fatalf("remain=%d err=%v, want 60 nil", remain, err)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("calls=%d want 2（1 次失败 + 1 次重试）", n)
	}
}

// TestSetBillingRetryDelayForTestRestores 钉住跨包测试钩子的契约：置短 + 还原。
// scheduler 包的失败遍历用例依赖它把每号 6s 的重试压到毫秒级；若钩子被改成
// 只置不还原（或还原函数失效），那个用例会以「假失败」的形式暴露，排查成本高，
// 所以在源头钉死。
func TestSetBillingRetryDelayForTestRestores(t *testing.T) {
	orig := billingRetryDelay
	restore := SetBillingRetryDelayForTest(time.Millisecond)
	if billingRetryDelay != time.Millisecond {
		t.Fatalf("置短后 delay=%v want 1ms", billingRetryDelay)
	}
	restore()
	if billingRetryDelay != orig {
		t.Fatalf("还原后 delay=%v want %v（必须回到调用前的值）", billingRetryDelay, orig)
	}
}