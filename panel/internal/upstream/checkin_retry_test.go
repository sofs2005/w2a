// checkin_retry_test.go 钉住面板侧签到/余额维护类计费调用的瞬时错误有界重试。
//
// 为什么面板侧也要有：用户在面板点的「签到」按钮走本包 DailyCheckin（面板持有
// 账号凭据、直连上游），不经网关，网关侧的重试覆盖不到这里。
package upstream

import (
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// shortBillingRetry 测试用重试间隔（生产 2s 会让单测秒级膨胀）。
func shortBillingRetry(t *testing.T) {
	t.Helper()
	old := billingRetryDelay
	billingRetryDelay = time.Millisecond
	t.Cleanup(func() { billingRetryDelay = old })
}

func TestRetryBillingTransientSucceedsAfter5xx(t *testing.T) {
	shortBillingRetry(t)
	var calls int32
	err := retryBillingTransient(func() error {
		if atomic.AddInt32(&calls, 1) == 1 {
			return &Error{Kind: ErrServer, Status: 500, Msg: "API request failed with status code: 500"}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("首次 500 应重试成功，err=%v", err)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("calls=%d want 2（1 次失败 + 1 次重试）", n)
	}
}

func TestRetryBillingTransientExhausted(t *testing.T) {
	shortBillingRetry(t)
	var calls int32
	err := retryBillingTransient(func() error {
		atomic.AddInt32(&calls, 1)
		return &Error{Kind: ErrServer, Status: 500, Msg: "boom"}
	})
	if err == nil {
		t.Fatal("持续 500 应返回错误")
	}
	if n := atomic.LoadInt32(&calls); n != 3 {
		t.Fatalf("calls=%d want 3（1 次 + 2 次重试封顶）", n)
	}
}

func TestRetryBillingTransientNetworkError(t *testing.T) {
	shortBillingRetry(t)
	var calls int32
	// 非 *Error 的传输层失败同样算瞬时，值得重试。
	err := retryBillingTransient(func() error {
		atomic.AddInt32(&calls, 1)
		return errors.New("dial tcp: connection reset by peer")
	})
	if err == nil {
		t.Fatal("持续网络错误应返回错误")
	}
	if n := atomic.LoadInt32(&calls); n != 3 {
		t.Fatalf("calls=%d want 3（网络抖动也重试）", n)
	}
}

func TestRetryBillingTransientNoRetryOn4xx(t *testing.T) {
	shortBillingRetry(t)
	var calls int32
	err := retryBillingTransient(func() error {
		atomic.AddInt32(&calls, 1)
		return &Error{Kind: ErrClient, Status: 403, Msg: "request illegal"}
	})
	if err == nil {
		t.Fatal("403 应返回错误")
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("calls=%d want 1（4xx 非瞬时，不重试）", n)
	}
}

func TestRetryBillingTransientNoRetryOnBusinessError(t *testing.T) {
	shortBillingRetry(t)
	var calls int32
	// 「今日已签到」是业务幂等拒绝，重试只会原样再失败一次。
	err := retryBillingTransient(func() error {
		atomic.AddInt32(&calls, 1)
		return &Error{Kind: ErrClient, Status: http.StatusOK, Msg: "今日已签到"}
	})
	if err == nil {
		t.Fatal("want business error")
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("calls=%d want 1（业务错误不重试）", n)
	}
}

func TestIsTransientBillingErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"server 5xx", &Error{Kind: ErrServer, Status: 500}, true},
		{"plain network", errors.New("i/o timeout"), true},
		{"client 4xx", &Error{Kind: ErrClient, Status: 400}, false},
		{"hard credit 402", &Error{Kind: ErrHardCredit, Status: 402}, false},
		{"soft rate 429", &Error{Kind: ErrSoftRate, Status: 429}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTransientBillingErr(tc.err); got != tc.want {
				t.Errorf("isTransientBillingErr(%v)=%v want %v", tc.err, got, tc.want)
			}
		})
	}
}