// Package ops GUI 的业务操作层：聚合网关状态与磁盘凭证，提供账号运维动作
// （签到 / 刷新 token / 猫猫旅行 / 积分查询）与批量任务。
package ops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"workbuddy2api-gui/internal/authstore"
	"workbuddy2api-gui/internal/config"
	"workbuddy2api-gui/internal/fsutil"
	"workbuddy2api-gui/internal/gateway"
	"workbuddy2api-gui/internal/pricing"
	"workbuddy2api-gui/internal/upstream"
)

// 只读 / 高危开关相关的哨兵错误，供 API 层映射为合适的状态码。
var (
	// ErrReadOnly 服务端处于只读模式。
	ErrReadOnly = errors.New("服务端已开启只读模式，写操作被禁用")
	// ErrDangerousDisabled 高危操作未解锁。
	ErrDangerousDisabled = errors.New("该操作属高危动作，需在服务端配置开启 dangerous_ops 后才能执行")
)

// 批量任务里账号之间的间隔，避免对上游造成瞬时压力（与网关调度器口径对齐）。
const (
	checkinAccountDelay = 200 * time.Millisecond
	travelAccountDelay  = 800 * time.Millisecond
)

// AccountView 账号的合并视图：磁盘凭证 + 网关运行态 + 积分缓存。
type AccountView struct {
	UID          string `json:"uid"`
	Nickname     string `json:"nickname"`
	EnterpriseID string `json:"enterprise_id"`
	Domain       string `json:"domain"`

	HasFile      bool   `json:"has_file"`
	FileName     string `json:"file_name"`
	ExpiresAt    int64  `json:"expires_at"`
	Expired      bool   `json:"expired"`
	NeedsRefresh bool   `json:"needs_refresh"`
	RefreshToken bool   `json:"has_refresh_token"`

	// gateway 侧运行态；InGateway=false 表示该账号在池中不存在。
	InGateway       bool      `json:"in_gateway"`
	Status          string    `json:"status"` // healthy | cooling | disabled | unknown
	Cooling         bool      `json:"cooling"`
	CoolKind        string    `json:"cool_kind,omitempty"`
	CoolRemaining   int64     `json:"cool_remaining_sec,omitempty"`
	Disabled        bool      `json:"disabled"`
	Reason          string    `json:"reason,omitempty"`
	InFlight        int       `json:"in_flight"`
	BreakerFails    int       `json:"breaker_fails"`
	BreakerUntil    time.Time `json:"breaker_until,omitempty"`
	SoftStreak      int       `json:"soft_streak,omitempty"`
	SuccessCount    int64     `json:"success_count"`
	ErrTotal        int64     `json:"err_total"`
	LastSuccessTime time.Time `json:"last_success,omitempty"`
	LastErrTime     time.Time `json:"last_err,omitempty"`

	// 积分：GatewayCredits 来自 /status（分钟级刷新），LiveCredits 来自主动查询（更准）。
	GatewayCredits int64      `json:"credits"`
	LiveCredits    *int64     `json:"live_credits,omitempty"`
	CreditsAt      *time.Time `json:"credits_at,omitempty"`
	// 积分到期：与 LiveCredits 同源同时刻（来自主动查询的积分包明细）。
	CreditsExpireAt *string `json:"credits_expire_at,omitempty"`
	CreditsExpiring *int64  `json:"credits_expiring,omitempty"`
}

// CreditsTotal 全局积分汇总。
type CreditsTotal struct {
	Remain   int64 `json:"remain"`
	Used     int64 `json:"used"`
	Size     int64 `json:"size"`
	Accounts int   `json:"accounts"`
	OK       int   `json:"ok"`
	Failed   int   `json:"failed"`
}

// Overview 仪表盘总览。
type Overview struct {
	GatewayURL     string          `json:"gateway_url"`
	GatewayOK      bool            `json:"gateway_ok"`
	GatewayError   string          `json:"gateway_error,omitempty"`
	Health         *gateway.Health `json:"health,omitempty"`
	Total          int             `json:"total"`
	Healthy        int             `json:"healthy"`
	Cooling        int             `json:"cooling"`
	Disabled       int             `json:"disabled"`
	InFlightFull   int             `json:"in_flight_full"`
	InFlight       int             `json:"in_flight"`
	StickySessions int             `json:"sticky_sessions"`
	RedisMode      string          `json:"redis_mode"`

	// 磁盘侧统计。
	FileCount  int      `json:"file_count"`
	Expired    int      `json:"expired"`
	Expiring   int      `json:"expiring"`
	Warnings   []string `json:"warnings,omitempty"`
	FileIssues []string `json:"file_issues,omitempty"`

	Credits CreditsTotal `json:"credits"`

	ReadOnly     bool `json:"read_only"`
	DangerousOps bool `json:"dangerous_ops"`

	ServerTime time.Time `json:"server_time"`
}

// OpResult 单账号操作结果。
type OpResult struct {
	UID     string         `json:"uid"`
	Action  string         `json:"action"`
	OK      bool           `json:"ok"`
	Message string         `json:"message"`
	Reward  int64          `json:"reward,omitempty"`
	Data    map[string]any `json:"data,omitempty"`
}

// creditCache 单账号积分缓存条目。
type creditCache struct {
	credits *upstream.Credits
	at      time.Time
	err     string
}

// Service 业务操作聚合入口。
type Service struct {
	cfg    *config.Config
	store  *authstore.Store
	gw     *gateway.Client
	up     *upstream.Client
	tasks  *TaskManager
	logins *LoginManager
	// pricing 官方价格表（把 token 用量换算成"走官方 API 要花多少钱"）。
	pricing *pricing.Table

	mu      sync.RWMutex
	credits map[string]creditCache
	promos  map[string]promoCache
	// creditLoading 标记「正在后台查询积分」的 uid，用于单飞（避免重复打上游）。
	creditLoading map[string]bool
}

// promoCache 促销缓存条目（按 realm 维度：促销是域级配置，同域账号看到的一致）。
type promoCache struct {
	promos []upstream.ModelPromotion
	at     time.Time
	err    string
}

// promoTTL 促销缓存时长。促销是营销配置，变化很慢，没必要每次开页面都打上游。
const promoTTL = 10 * time.Minute

// promoErrTTL 促销**失败**的缓存时长，比成功短得多。
//
// 失败往往是一次网络抖动或上游限流，不该让它把「优惠」列占掉 10 分钟；
// 但也不能不缓存 —— 上游一直挂着时，每次开页面都同步等一次超时会很难受。
const promoErrTTL = time.Minute

// realmOfAccount 从账号 domain 反推域：含 workbuddy.ai 为 global，其余（copilot.tencent.com /
// codebuddy.cn）为 cn。与网关 auth.Realm() 的判定口径一致。
func realmOfAccount(a *authstore.Account) string {
	if a != nil && strings.Contains(strings.ToLower(a.Domain), "workbuddy.ai") {
		return "global"
	}
	return "cn"
}

// PromotionsForRealm 返回某域的模型促销配置（best-effort）。
//
// 每域挑一个未过期账号去取上游 /v3/config —— 网关不解析 modelPromotions，故 GUI 直连。
// 命中缓存（promoTTL）时零上游调用；失败返回 (nil, 错误文案) 且不阻断调用方，
// 促销拿不到不该让整个模型页打不开。
func (s *Service) PromotionsForRealm(realm string) ([]upstream.ModelPromotion, string) {
	s.mu.RLock()
	c, ok := s.promos[realm]
	s.mu.RUnlock()
	if ok {
		ttl := promoTTL
		if c.err != "" {
			ttl = promoErrTTL
		}
		if time.Since(c.at) < ttl {
			return c.promos, c.err
		}
	}

	accounts, _ := s.store.List()
	var acct *authstore.Account
	for _, a := range accounts {
		if realmOfAccount(a) == realm && !a.Expired() {
			acct = a
			break
		}
	}
	if acct == nil {
		msg := "该域没有可用账号，无法查询促销"
		s.mu.Lock()
		s.promos[realm] = promoCache{at: time.Now(), err: msg}
		s.mu.Unlock()
		return nil, msg
	}
	if acct.NeedsRefresh(10 * time.Minute) {
		// 过期前先续期，避免拿一个马上失效的 token 去打上游；失败不阻断（可能仍能读）。
		_ = s.refreshAccount(acct)
	}

	promos, err := s.up.Promotions(acct)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.promos[realm] = promoCache{at: time.Now(), err: err.Error()}
		return nil, err.Error()
	}
	s.promos[realm] = promoCache{promos: promos, at: time.Now()}
	return promos, ""
}

// New 构建服务。
func New(cfg *config.Config, store *authstore.Store, gw *gateway.Client, up *upstream.Client) *Service {
	return &Service{
		cfg:           cfg,
		store:         store,
		gw:            gw,
		up:            up,
		tasks:         NewTaskManager(),
		logins:        NewLoginManager(up),
		pricing:       pricing.New(cfg.PricingFile),
		credits:       map[string]creditCache{},
		promos:        map[string]promoCache{},
		creditLoading: map[string]bool{},
	}
}

// Pricing 返回官方价格表。
func (s *Service) Pricing() *pricing.Table { return s.pricing }

// Config 返回当前配置。
func (s *Service) Config() *config.Config { return s.cfg }

// Tasks 返回任务管理器。
func (s *Service) Tasks() *TaskManager { return s.tasks }

// Logins 返回登录会话管理器。
func (s *Service) Logins() *LoginManager { return s.logins }

// Gateway 返回网关客户端。
func (s *Service) Gateway() *gateway.Client { return s.gw }

// Upstream 返回上游客户端。
func (s *Service) Upstream() *upstream.Client { return s.up }

// Store 返回凭证存储。
func (s *Service) Store() *authstore.Store { return s.store }

// EnsureWritable 写操作前置校验（导出供 API 层复用）。
func (s *Service) EnsureWritable() error { return s.ensureWritable() }

// ensureWritable 写操作前置校验。
func (s *Service) ensureWritable() error {
	if s.cfg.ReadOnly {
		return ErrReadOnly
	}
	return nil
}

// ensureDangerous 高危操作前置校验。
func (s *Service) ensureDangerous() error {
	if err := s.ensureWritable(); err != nil {
		return err
	}
	if !s.cfg.DangerousOps {
		return ErrDangerousDisabled
	}
	return nil
}

// ---------------------------------------------------------------------------
// 账号视图聚合
// ---------------------------------------------------------------------------

// Accounts 返回「磁盘凭证 ∪ 网关运行态」的合并账号列表。
// 网关不可达时不报错：磁盘侧账号仍返回，状态标记为 unknown。
func (s *Service) Accounts(ctx context.Context) ([]AccountView, *gateway.Status, []string, error) {
	diskAccounts, fileIssues := s.store.List()
	status, gwErr := s.gw.Status(ctx)

	byUID := map[string]*AccountView{}
	for _, a := range diskAccounts {
		v := &AccountView{
			UID:          a.UID,
			Nickname:     a.Nickname,
			EnterpriseID: a.EnterpriseID,
			Domain:       a.Domain,
			HasFile:      true,
			FileName:     baseName(a.FilePath),
			ExpiresAt:    a.ExpiresAt,
			Expired:      a.Expired(),
			NeedsRefresh: a.NeedsRefresh(10 * time.Minute),
			RefreshToken: strings.TrimSpace(a.RefreshToken) != "",
			Status:       "unknown",
		}
		byUID[a.UID] = v
	}

	if status != nil {
		for i := range status.Accounts {
			ga := status.Accounts[i]
			v, ok := byUID[ga.UID]
			if !ok {
				// 池中存在但磁盘无凭证（凭证文件被删/损坏但池未重启）。
				v = &AccountView{UID: ga.UID, Status: "unknown"}
				byUID[ga.UID] = v
			}
			v.InGateway = true
			v.Cooling = ga.Cooling
			v.CoolKind = ga.CoolKind
			v.CoolRemaining = ga.CoolRemaining
			v.Disabled = ga.Disabled
			v.Reason = ga.Reason
			v.InFlight = ga.InFlight
			v.BreakerFails = ga.BreakerFails
			v.BreakerUntil = ga.BreakerUntil
			v.SoftStreak = ga.SoftStreak
			v.SuccessCount = ga.SuccessCount
			v.ErrTotal = ga.ErrTotal
			v.LastSuccessTime = ga.LastSuccessTime
			v.LastErrTime = ga.LastErrTime
			v.GatewayCredits = ga.Credits
			if v.Nickname == "" {
				v.Nickname = ga.Nickname
			}
			switch {
			case ga.Disabled:
				v.Status = "disabled"
			case ga.Cooling:
				v.Status = "cooling"
			default:
				v.Status = "healthy"
			}
		}
	}

	// 合并积分缓存。
	s.mu.RLock()
	for uid, c := range s.credits {
		if v, ok := byUID[uid]; ok && c.credits != nil {
			remain := c.credits.Remain
			at := c.at
			v.LiveCredits = &remain
			v.CreditsAt = &at
			if c.credits.ExpiresAt != "" {
				exp := c.credits.ExpiresAt
				v.CreditsExpireAt = &exp
			}
			expiring := c.credits.ExpiringRemain
			v.CreditsExpiring = &expiring
		}
	}
	s.mu.RUnlock()

	out := make([]AccountView, 0, len(byUID))
	for _, v := range byUID {
		if v.Status == "unknown" {
			// 磁盘有凭证但网关未见过（或网关不可达）：按 token 状态给个更友好的结论。
			if !v.HasFile {
				v.Status = "missing_credential"
			} else if v.Expired {
				v.Status = "token_expired"
			} else if status == nil {
				v.Status = "gateway_unreachable"
			}
		}
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Nickname != out[j].Nickname {
			return out[i].Nickname < out[j].Nickname
		}
		return out[i].UID < out[j].UID
	})
	_ = gwErr // 网关错误通过 status==nil 体现，由 Overview 暴露详情
	return out, status, fileIssues, nil
}

// Overview 汇总仪表盘数据。
func (s *Service) Overview(ctx context.Context) *Overview {
	accounts, status, fileIssues, _ := s.Accounts(ctx)
	ov := &Overview{
		GatewayURL:   s.gw.BaseURL(),
		FileCount:    len(accounts),
		ReadOnly:     s.cfg.ReadOnly,
		DangerousOps: s.cfg.DangerousOps,
		ServerTime:   time.Now(),
		FileIssues:   fileIssues,
	}
	// 网关可达性 + 身份校验。
	if h, err := s.gw.Health(ctx); err == nil {
		ov.GatewayOK = true
		ov.Health = h
	} else {
		ov.GatewayError = err.Error()
	}
	if status != nil {
		ov.Total = status.Total
		ov.Healthy = status.Healthy
		ov.Cooling = status.Cooling
		ov.Disabled = status.Disabled
		ov.InFlightFull = status.InFlightFull
		ov.StickySessions = status.StickySessions
		ov.RedisMode = status.RedisMode
	}
	var inFlight int
	for _, a := range accounts {
		inFlight += a.InFlight
		if a.Expired {
			ov.Expired++
		} else if a.NeedsRefresh {
			ov.Expiring++
		}
		if !a.HasFile && a.InGateway {
			ov.Warnings = append(ov.Warnings,
				fmt.Sprintf("账号 %s 在网关池中但磁盘无凭证文件，重启后将消失", shortUID(a.UID)))
		}
		if a.HasFile && !a.InGateway && status != nil {
			ov.Warnings = append(ov.Warnings, fmt.Sprintf("账号 %s 有凭证文件但不在网关池中：%s",
				shortUID(a.UID), s.credentialLoadHint(a.UID)))
		}
		// 积分汇总口径：主动查询结果优先，其次用网关 /status 里缓存的积分。
		// 两者都没有（例如凭证文件存在但网关未加载该账号）才算「未取到」，据实计入 failed。
		ov.Credits.Accounts++
		switch {
		case a.LiveCredits != nil:
			ov.Credits.Remain += *a.LiveCredits
			ov.Credits.OK++
		case a.InGateway:
			ov.Credits.Remain += a.GatewayCredits
			ov.Credits.OK++
		default:
			ov.Credits.Failed++
		}
	}
	ov.InFlight = inFlight
	if ov.RedisMode == "" {
		ov.RedisMode = "noop"
	}
	s.mu.RLock()
	totalSize := int64(0)
	used := int64(0)
	for _, c := range s.credits {
		if c.credits != nil {
			totalSize += c.credits.Size
			used += c.credits.Used
		}
	}
	s.mu.RUnlock()
	ov.Credits.Size = totalSize
	ov.Credits.Used = used
	ov.Credits.Failed = ov.Credits.Accounts - ov.Credits.OK
	return ov
}

// 积分缓存的「新鲜」时长。
//
// 取值权衡：积分只在调用模型时消耗，变化不快；但用户刚用过就打开页面会希望
// 看到接近实时的数字。5 分钟既避免了每次翻页都打上游（N 个账号 × 一次请求），
// 又不会让数字陈旧到误导。
const creditTTL = 5 * time.Minute

// EnsureCredits 为「尚无缓存或缓存已过期」的账号在后台补齐积分。
//
// 设计取态：
//   - **异步**：调用方（Accounts）是页面请求路径，不能为上游查询阻塞数秒。
//   - **单飞**：同一 uid 已有在途查询时跳过，避免用户狂点刷新把上游打爆。
//   - **只查缺失**：TTL 内的缓存直接复用，不重复打上游。
//
// 这是 issue #8 的修复：外部渠道（如 workbuddy-manager）写入 auths/ 的账号，
// 网关池里的 credits 初值是 0，而网关只在签到任务里才更新它——于是账号页
// 一直显示 0。现在控制台自己直连上游补齐，不依赖网关是否跑过签到。
func (s *Service) EnsureCredits(views []AccountView) {
	now := time.Now()

	s.mu.Lock()
	if s.creditLoading == nil {
		s.creditLoading = map[string]bool{}
	}
	var todo []string
	for _, v := range views {
		if !v.HasFile {
			continue // 无凭证文件，查不了
		}
		if c, ok := s.credits[v.UID]; ok && c.credits != nil && now.Sub(c.at) < creditTTL {
			continue // 缓存新鲜
		}
		if s.creditLoading[v.UID] {
			continue // 已有在途查询
		}
		s.creditLoading[v.UID] = true
		todo = append(todo, v.UID)
	}
	s.mu.Unlock()

	if len(todo) == 0 {
		return
	}
	go func() {
		// 串行 + 间隔：与批量查询保持一致的限速口径，避免触发上游风控。
		for i, uid := range todo {
			if i > 0 {
				time.Sleep(creditQueryInterval)
			}
			ctx, cancel := context.WithTimeout(context.Background(), creditQueryTimeout)
			_, _ = s.CreditsFor(ctx, uid) // 结果已进缓存，失败也会写入错误
			cancel()

			s.mu.Lock()
			delete(s.creditLoading, uid)
			s.mu.Unlock()
		}
	}()
}

// 后台补积分任务的限速与超时。
const (
	creditQueryInterval = 300 * time.Millisecond
	creditQueryTimeout  = 20 * time.Second
)

// ---------------------------------------------------------------------------
// 单账号操作
// ---------------------------------------------------------------------------

// loadAccount 读取账号凭证。
func (s *Service) loadAccount(uid string) (*authstore.Account, error) {
	a, err := s.store.Get(uid)
	if err != nil {
		return nil, describeAccountLoadError(uid, err)
	}
	return a, nil
}

// describeAccountLoadError 把读凭证的底层错误转成面向用户的文案。
//
// 为什么不直接把 err 抛出去：os.ReadFile 的错误里带着服务器上的绝对路径
// （如 /root/workbuddy2api/auths/...），对使用者是噪音，也会泄露部署细节。
func describeAccountLoadError(uid string, err error) error {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("账号不存在：%s（凭证文件已被删除？）", shortUID(uid))
	case os.IsPermission(err):
		return fmt.Errorf("没有读取账号 %s 凭证文件的权限（请检查文件权限）", shortUID(uid))
	default:
		if authstore.ValidUID(uid) == false {
			return fmt.Errorf("非法 uid：%q", uid)
		}
		return fmt.Errorf("读取账号 %s 失败：%v", shortUID(uid), err)
	}
}

// Checkin 对单账号执行签到 + 余额刷新。
func (s *Service) Checkin(ctx context.Context, uid string) (*OpResult, error) {
	if err := s.ensureWritable(); err != nil {
		return nil, err
	}
	a, err := s.loadAccount(uid)
	if err != nil {
		return nil, err
	}
	res := &OpResult{UID: uid, Action: "checkin", OK: true}

	// 先刷新过期 token，否则签到必然 401。
	if a.NeedsRefresh(10 * time.Minute) {
		if err := s.refreshAccount(a); err != nil {
			return &OpResult{UID: uid, Action: "checkin", OK: false,
				Message: "刷新 token 失败: " + err.Error()}, nil
		}
	}
	cr, err := s.up.DailyCheckin(a)
	switch {
	case err != nil:
		res.OK = false
		res.Message = "签到失败: " + err.Error()
	case cr.Already:
		res.Message = "今日已签到"
	default:
		res.Message = cr.Message
	}
	// 无论签到结果如何，都刷新一次余额（可能已恢复额度）。
	if credits, cerr := s.up.UserResource(a); cerr == nil {
		s.setCreditCache(uid, credits)
		res.Data = map[string]any{"credits": credits}
	}
	return res, nil
}

// RefreshToken 刷新单账号 token 并落盘。
func (s *Service) RefreshToken(ctx context.Context, uid string) (*OpResult, error) {
	if err := s.ensureWritable(); err != nil {
		return nil, err
	}
	a, err := s.loadAccount(uid)
	if err != nil {
		return nil, err
	}
	before := a.ExpiresAt
	if err := s.refreshAccount(a); err != nil {
		return &OpResult{UID: uid, Action: "refresh", OK: false, Message: err.Error()}, nil
	}
	return &OpResult{
		UID: uid, Action: "refresh", OK: true,
		Message: fmt.Sprintf("token 已刷新，有效期至 %s", a.ExpiresAtTime().Format("2006-01-02 15:04:05")),
		Data: map[string]any{
			"expires_at":       a.ExpiresAt,
			"expires_before":   before,
			"expires_at_human": a.ExpiresAtTime().Format(time.RFC3339),
		},
	}, nil
}

// refreshAccount 刷新并原子落盘。
func (s *Service) refreshAccount(a *authstore.Account) error {
	if err := s.up.RefreshToken(a); err != nil {
		return err
	}
	if err := s.store.Save(a); err != nil {
		return fmt.Errorf("刷新成功但落盘失败: %w", err)
	}
	return nil
}

// Travel 对单账号推进一趟猫猫旅行。
func (s *Service) Travel(ctx context.Context, uid string) (*OpResult, error) {
	if err := s.ensureWritable(); err != nil {
		return nil, err
	}
	a, err := s.loadAccount(uid)
	if err != nil {
		return nil, err
	}
	if a.NeedsRefresh(10 * time.Minute) {
		if err := s.refreshAccount(a); err != nil {
			return &OpResult{UID: uid, Action: "travel", OK: false,
				Message: "刷新 token 失败: " + err.Error()}, nil
		}
	}
	tr, err := s.up.TravelOnce(a)
	if err != nil {
		msg := tr.Message
		if msg == "" {
			msg = err.Error()
		}
		return &OpResult{UID: uid, Action: "travel", OK: false, Message: msg}, nil
	}
	ok := tr.Action != "error"
	return &OpResult{
		UID: uid, Action: "travel", OK: ok,
		Message: tr.Message, Reward: tr.Reward,
		Data: map[string]any{"travel_action": tr.Action, "buddy": tr.Buddy},
	}, nil
}

// CreditsFor 主动查询单账号积分（结果进缓存）。
func (s *Service) CreditsFor(ctx context.Context, uid string) (*upstream.Credits, error) {
	a, err := s.loadAccount(uid)
	if err != nil {
		return nil, err
	}
	if a.NeedsRefresh(10 * time.Minute) {
		if err := s.refreshAccount(a); err != nil {
			return nil, fmt.Errorf("刷新 token 失败: %w", err)
		}
	}
	c, err := s.up.UserResource(a)
	if err != nil {
		s.mu.Lock()
		s.credits[uid] = creditCache{at: time.Now(), err: err.Error()}
		s.mu.Unlock()
		return nil, err
	}
	s.setCreditCache(uid, c)
	return c, nil
}

// Profile 返回单账号的完整详情（含实时积分与猫档案）。
//
// silent=true 时跳过所有可能失败的上游调用，只返回本地已知信息。
func (s *Service) Profile(ctx context.Context, uid string, silent bool) (map[string]any, error) {
	accounts, _, _, _ := s.Accounts(ctx)
	var view *AccountView
	for i := range accounts {
		if accounts[i].UID == uid {
			view = &accounts[i]
			break
		}
	}
	if view == nil {
		return nil, fmt.Errorf("账号不存在: %s", uid)
	}
	out := map[string]any{"account": view}
	if silent {
		return out, nil
	}
	a, err := s.store.Get(uid)
	if err != nil {
		out["upstream_error"] = "读取凭证失败: " + err.Error()
		return out, nil
	}
	if a.NeedsRefresh(10 * time.Minute) {
		if err := s.refreshAccount(a); err != nil {
			out["upstream_error"] = "刷新 token 失败: " + err.Error()
			return out, nil
		}
	}
	if c, err := s.up.UserResource(a); err == nil {
		s.setCreditCache(uid, c)
		out["credits"] = c
	} else {
		out["credits_error"] = err.Error()
	}
	if b, err := s.up.BuddyInfo(a); err == nil {
		out["buddy"] = b
	} else {
		out["buddy_error"] = err.Error()
	}
	if ts, err := s.up.TravelStatus(a); err == nil {
		out["travel"] = ts
	} else {
		out["travel_error"] = err.Error()
	}
	return out, nil
}

// setCreditCache 写入积分缓存。
func (s *Service) setCreditCache(uid string, c *upstream.Credits) {
	s.mu.Lock()
	s.credits[uid] = creditCache{credits: c, at: time.Now()}
	s.mu.Unlock()
}

// ---------------------------------------------------------------------------
// 批量操作（异步任务）
// ---------------------------------------------------------------------------

// targetAccounts 解析批量操作的目标账号：uids 为空表示全量。
func (s *Service) targetAccounts(ctx context.Context, uids []string) ([]*authstore.Account, error) {
	if len(uids) > 0 {
		out := make([]*authstore.Account, 0, len(uids))
		for _, uid := range uids {
			a, err := s.store.Get(uid)
			if err != nil {
				return nil, fmt.Errorf("账号 %s: %w", shortUID(uid), err)
			}
			out = append(out, a)
		}
		return out, nil
	}
	all, _ := s.store.List()
	return all, nil
}

// BatchCheckin 批量签到 + 余额刷新（异步任务）。
func (s *Service) BatchCheckin(ctx context.Context, uids []string) (*TaskView, error) {
	if err := s.ensureWritable(); err != nil {
		return nil, err
	}
	accounts, err := s.targetAccounts(ctx, uids)
	if err != nil {
		return nil, err
	}
	return s.tasks.New("checkin", fmt.Sprintf("批量签到（%d 个账号）", len(accounts)), func(t *Task) {
		for i, a := range accounts {
			if i > 0 {
				time.Sleep(checkinAccountDelay)
			}
			t.addItem(s.checkinOne(a))
		}
	}), nil
}

// checkinOne 单账号签到（批量任务内部使用）。
func (s *Service) checkinOne(a *authstore.Account) TaskItem {
	item := TaskItem{
		UID: a.UID, Nickname: a.Nickname, Action: "checkin",
		OK: true, StartedAt: time.Now(),
	}
	defer func() { item.EndedAt = time.Now() }()
	if a.NeedsRefresh(10 * time.Minute) {
		if err := s.refreshAccount(a); err != nil {
			item.OK = false
			item.Message = "刷新 token 失败: " + err.Error()
			return item
		}
	}
	cr, err := s.up.DailyCheckin(a)
	switch {
	case err != nil:
		item.OK = false
		item.Message = err.Error()
	case cr.Already:
		item.Message = "今日已签到"
	default:
		item.Message = cr.Message
	}
	if c, cerr := s.up.UserResource(a); cerr == nil {
		s.setCreditCache(a.UID, c)
		item.Message += fmt.Sprintf("｜余额 %d", c.Remain)
	}
	return item
}

// BatchRefresh 批量刷新 token（异步任务）。
func (s *Service) BatchRefresh(ctx context.Context, uids []string) (*TaskView, error) {
	if err := s.ensureWritable(); err != nil {
		return nil, err
	}
	accounts, err := s.targetAccounts(ctx, uids)
	if err != nil {
		return nil, err
	}
	return s.tasks.New("refresh", fmt.Sprintf("批量刷新 Token（%d 个账号）", len(accounts)), func(t *Task) {
		for i, a := range accounts {
			if i > 0 {
				time.Sleep(checkinAccountDelay)
			}
			start := time.Now()
			item := TaskItem{UID: a.UID, Nickname: a.Nickname, Action: "refresh", OK: true, StartedAt: start}
			if err := s.refreshAccount(a); err != nil {
				item.OK = false
				item.Message = err.Error()
			} else {
				item.Message = "有效期至 " + a.ExpiresAtTime().Format("2006-01-02 15:04:05")
			}
			item.EndedAt = time.Now()
			t.addItem(item)
		}
	}), nil
}

// BatchTravel 批量推进猫猫旅行（异步任务）。
func (s *Service) BatchTravel(ctx context.Context, uids []string) (*TaskView, error) {
	if err := s.ensureWritable(); err != nil {
		return nil, err
	}
	accounts, err := s.targetAccounts(ctx, uids)
	if err != nil {
		return nil, err
	}
	return s.tasks.New("travel", fmt.Sprintf("批量猫猫旅行（%d 个账号）", len(accounts)), func(t *Task) {
		for i, a := range accounts {
			if i > 0 {
				time.Sleep(travelAccountDelay)
			}
			start := time.Now()
			item := TaskItem{UID: a.UID, Nickname: a.Nickname, Action: "travel", OK: true, StartedAt: start}
			if a.NeedsRefresh(10 * time.Minute) {
				if err := s.refreshAccount(a); err != nil {
					item.OK = false
					item.Message = "刷新 token 失败: " + err.Error()
					item.EndedAt = time.Now()
					t.addItem(item)
					continue
				}
			}
			tr, err := s.up.TravelOnce(a)
			switch {
			case err != nil:
				item.OK = false
				if tr != nil && tr.Message != "" {
					item.Message = tr.Message
				} else {
					item.Message = err.Error()
				}
			default:
				item.Message = tr.Message
				item.Reward = tr.Reward
			}
			item.EndedAt = time.Now()
			t.addItem(item)
		}
	}), nil
}

// BatchCredits 批量查询积分（异步任务，结果写缓存）。
func (s *Service) BatchCredits(ctx context.Context, uids []string) (*TaskView, error) {
	accounts, err := s.targetAccounts(ctx, uids)
	if err != nil {
		return nil, err
	}
	return s.tasks.New("credits", fmt.Sprintf("批量查询积分（%d 个账号）", len(accounts)), func(t *Task) {
		for i, a := range accounts {
			if i > 0 {
				time.Sleep(checkinAccountDelay)
			}
			start := time.Now()
			item := TaskItem{UID: a.UID, Nickname: a.Nickname, Action: "credits", OK: true, StartedAt: start}
			if a.NeedsRefresh(10 * time.Minute) {
				if err := s.refreshAccount(a); err != nil {
					item.OK = false
					item.Message = "刷新 token 失败: " + err.Error()
					item.EndedAt = time.Now()
					t.addItem(item)
					continue
				}
			}
			c, err := s.up.UserResource(a)
			if err != nil {
				item.OK = false
				item.Message = err.Error()
			} else {
				s.setCreditCache(a.UID, c)
				item.Message = fmt.Sprintf("剩余 %d / 总量 %d（%d 个套餐）", c.Remain, c.Size, c.Packages)
			}
			item.EndedAt = time.Now()
			t.addItem(item)
		}
	}), nil
}

// ---------------------------------------------------------------------------
// 账号增删改
// ---------------------------------------------------------------------------

// DeleteAccount 删除账号凭证（高危：不可从上游恢复）。
func (s *Service) DeleteAccount(ctx context.Context, uid string) error {
	if err := s.ensureDangerous(); err != nil {
		return err
	}
	return s.store.Delete(uid)
}

// StateFileInfo 池状态文件信息（只读展示）。
type StateFileInfo struct {
	Path     string    `json:"path"`
	Exists   bool      `json:"exists"`
	Size     int64     `json:"size"`
	ModTime  time.Time `json:"mod_time,omitempty"`
	Accounts int       `json:"accounts"`
	Err      string    `json:"error,omitempty"`
}

// credentialLoadHint 解释「磁盘有凭证文件、但网关池里没有」该怎么处理。
//
// 控制台只看得到自己这侧的目录，看不到网关那侧的文件系统，所以成因得靠现有线索
// 推，而不是一口咬定「权限让网关读不到」。按线索分三种：
//
//  1. 文件对同组/其他用户可读 —— 网关哪怕以别的用户运行也读得到，成因只剩
//     「网关还没重新扫描目录」（网关只在启动时读一次 auths/）。
//  2. 配了 auth_owner_uid/auth_owner_gid，但文件属主没跟着变 —— 落盘那步没生效。
//  3. 文件属主是 root 且仅 root 可读 —— 真正的隐患：非 root 的网关进程一定读不到
//     （容器部署正是这种：面板以 root 落盘，网关以 uid 10001 的 app 运行）。
//     其余属主（比如本控制台进程自己）则同机同用户的网关读得到。
//
// 注意别建议 chown 到文件当前属主 —— 那是空操作，等于没给建议。
func (s *Service) credentialLoadHint(uid string) string {
	p := s.store.PathFor(uid)
	st, err := os.Stat(p)
	if p == "" || err != nil {
		return "需重启网关加载"
	}
	mode := st.Mode().Perm()
	file := "auths/workbuddy-" + shortUID(uid) + ".json"
	uid2, gid := s.store.Owner()
	fuid, fgid, haveOwner := fsutil.FileOwner(st)
	owner := fmt.Sprintf("%d:%d", fuid, fgid)
	if !haveOwner {
		owner = "未知"
	}

	if mode&0o044 != 0 {
		return "文件对同组/其他用户可读，网关读得到 —— 多半只是还没重新扫描目录，到「系统」页重启网关即可"
	}
	// 配了目标属主：属主是否真的跟着变了，决定是「落盘那步没生效」还是「权限没问题」。
	// 只看「配置有没有设」会误报 —— 配置生效时文件本来就该是那个属主。
	if uid2 >= 0 || gid >= 0 {
		matched := haveOwner && (uid2 < 0 || fuid == uid2) && (gid < 0 || fgid == gid)
		if matched {
			return "文件属主与配置的 auth_owner_uid/auth_owner_gid 一致，权限没问题 —— " +
				"多半只是还没重新扫描目录，到「系统」页重启网关即可"
		}
		return fmt.Sprintf("已配置 auth_owner_uid=%d/auth_owner_gid=%d，但该文件属主是 %s，"+
			"落盘时未改成，请检查挂载目录权限", uid2, gid, owner)
	}
	if haveOwner && fuid == 0 {
		return fmt.Sprintf("文件属主是 root 且仅属主可读（%o）。若网关以其他用户运行（容器部署：官方镜像是 "+
			"uid 10001 的 app），它会因权限拒绝跳过该文件，重启无效 —— 把 auth_owner_uid/auth_owner_gid "+
			"设为网关进程的 uid:gid 后重新落盘，或直接 chmod 0644 %s", mode, file)
	}
	return fmt.Sprintf("文件属主是 %s 且仅属主可读（%o）。若网关与本控制台同机且同用户，读得到，"+
		"多半只是还没重新扫描目录，重启网关即可；若两者各读各的 auths 目录（跨机部署），"+
		"文件不在网关那台机器上，需先把凭证部署过去", owner, mode)
}

func shortUID(uid string) string {
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}

func baseName(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}
