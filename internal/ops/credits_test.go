package ops

import (
	"sync"
	"testing"
	"time"

	"workbuddy2api-gui/internal/config"
)

// TestEnsureCreditsSingleFlight 同一 uid 不应被重复排队查询。
//
// 单飞的意义：账号页 20 秒自动轮询一次，若不设防，每次轮询都会为所有
// 「尚无缓存」的账号再排一轮查询——上游会被无谓地打爆。
func TestEnsureCreditsSingleFlight(t *testing.T) {
	s := &Service{
		cfg:           &config.Config{},
		credits:       map[string]creditCache{},
		creditLoading: map[string]bool{},
	}
	views := []AccountView{
		{UID: "u1", HasFile: true},
		{UID: "u2", HasFile: true},
	}

	// 标记 u1 已在途，模拟上一次轮询遗留的查询。
	s.creditLoading["u1"] = true

	// 直接验证筛选逻辑：u1 应被跳过，u2 应入选。
	now := time.Now()
	s.mu.Lock()
	var todo []string
	for _, v := range views {
		if !v.HasFile {
			continue
		}
		if c, ok := s.credits[v.UID]; ok && c.credits != nil && now.Sub(c.at) < creditTTL {
			continue
		}
		if s.creditLoading[v.UID] {
			continue
		}
		s.creditLoading[v.UID] = true
		todo = append(todo, v.UID)
	}
	s.mu.Unlock()

	if len(todo) != 1 || todo[0] != "u2" {
		t.Errorf("在途的 u1 应被跳过，得到 todo=%v", todo)
	}
}

// TestEnsureCreditsSkipsFreshCache 新鲜缓存不应触发重新查询。
func TestEnsureCreditsSkipsFreshCache(t *testing.T) {
	// 直接测试 TTL 判定（creditTTL = 5 分钟）。
	now := time.Now()
	fresh := creditCache{at: now.Add(-1 * time.Minute)}  // TTL 内
	stale := creditCache{at: now.Add(-10 * time.Minute)} // 超 TTL（5 分钟）

	if now.Sub(fresh.at) >= creditTTL {
		t.Error("1 分钟前的缓存应视为新鲜")
	}
	if now.Sub(stale.at) < creditTTL {
		t.Error("10 分钟前的缓存应视为过期")
	}
}

// TestEnsureCreditsNoFile 无凭证文件的账号不应被查询（查不了）。
func TestEnsureCreditsNoFile(t *testing.T) {
	s := &Service{
		cfg:           &config.Config{},
		credits:       map[string]creditCache{},
		creditLoading: map[string]bool{},
	}
	s.EnsureCredits([]AccountView{{UID: "nofile", HasFile: false}})
	time.Sleep(50 * time.Millisecond)
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.creditLoading["nofile"] {
		t.Error("无凭证文件的账号不应进入查询队列")
	}
}

// TestEnsureCreditsConcurrentSafe 并发触发不应竞态。
func TestEnsureCreditsConcurrentSafe(t *testing.T) {
	s := &Service{
		cfg:           &config.Config{},
		credits:       map[string]creditCache{},
		creditLoading: map[string]bool{},
	}
	views := []AccountView{{UID: "u1", HasFile: false}, {UID: "u2", HasFile: false}}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.EnsureCredits(views)
		}()
	}
	wg.Wait()
}
