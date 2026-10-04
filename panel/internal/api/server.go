// Package api GUI 的 HTTP 接口层：面板鉴权 + REST API + SPA 静态资源。
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"workbuddy2api-gui/internal/config"
	"workbuddy2api-gui/internal/gateway"
	"workbuddy2api-gui/internal/ops"
	"workbuddy2api-gui/internal/pricing"
	"workbuddy2api-gui/internal/upstream"
)

// Server API 服务器。
type Server struct {
	cfg      *config.Config
	svc      *ops.Service
	sessions *SessionStore
	static   http.Handler
	version  string
	started  time.Time
}

// NewServer 构建 API 服务器。
func NewServer(cfg *config.Config, svc *ops.Service, static http.Handler, version string) *Server {
	return &Server{
		cfg:      cfg,
		svc:      svc,
		sessions: NewSessionStore(cfg.UI.TTL),
		static:   static,
		version:  version,
		started:  time.Now(),
	}
}

// Handler 返回已装配中间件的根 handler。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// ── 会话 ──────────────────────────────────────────────
	mux.HandleFunc("GET /api/session", s.handleSessionInfo)
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.HandleFunc("POST /api/password", s.handleChangePassword)

	// ── 总览 / 账号 ───────────────────────────────────────
	mux.HandleFunc("GET /api/overview", s.handleOverview)
	mux.HandleFunc("GET /api/accounts", s.handleAccounts)
	mux.HandleFunc("GET /api/accounts/{uid}", s.handleAccountDetail)
	mux.HandleFunc("DELETE /api/accounts/{uid}", s.handleAccountDelete)
	mux.HandleFunc("POST /api/accounts/import", s.handleAccountImport)
	mux.HandleFunc("POST /api/accounts/{uid}/checkin", s.handleAccountCheckin)
	mux.HandleFunc("POST /api/accounts/{uid}/refresh", s.handleAccountRefresh)
	mux.HandleFunc("POST /api/accounts/{uid}/travel", s.handleAccountTravel)
	mux.HandleFunc("POST /api/accounts/{uid}/credits", s.handleAccountCredits)

	// ── 批量任务 ──────────────────────────────────────────
	mux.HandleFunc("POST /api/tasks/checkin", s.handleBatchCheckin)
	mux.HandleFunc("POST /api/tasks/refresh", s.handleBatchRefresh)
	mux.HandleFunc("POST /api/tasks/travel", s.handleBatchTravel)
	mux.HandleFunc("POST /api/tasks/credits", s.handleBatchCredits)
	mux.HandleFunc("GET /api/tasks", s.handleTaskList)
	mux.HandleFunc("GET /api/tasks/{id}", s.handleTaskDetail)

	// ── 网页登录 ──────────────────────────────────────────
	mux.HandleFunc("POST /api/login/start", s.handleLoginStart)
	mux.HandleFunc("GET /api/login/{id}", s.handleLoginStatus)
	mux.HandleFunc("POST /api/login/{id}/poll", s.handleLoginPoll)
	mux.HandleFunc("POST /api/login/{id}/cancel", s.handleLoginCancel)

	// ── 模型 / 聊天测试台 ─────────────────────────────────
	mux.HandleFunc("GET /api/models", s.handleModels)
	// ── 请求统计（按模型聚合，数据源为网关 /v1/stats）─────
	mux.HandleFunc("GET /api/stats", s.handleStats)
	mux.HandleFunc("POST /api/stats/reset", s.handleStatsReset)
	// 官方价格表编辑（统计页换算用）
	mux.HandleFunc("PUT /api/pricing", s.handlePricingUpdate)
	mux.HandleFunc("DELETE /api/pricing/{model}", s.handlePricingDelete)
	mux.HandleFunc("POST /api/chat", s.handleChat)
	mux.HandleFunc("POST /api/chat/stream", s.handleChatStream)

	// ── 网关配置 ──────────────────────────────────────────
	mux.HandleFunc("GET /api/config", s.handleConfigGet)
	mux.HandleFunc("PUT /api/config", s.handleConfigPut)
	mux.HandleFunc("POST /api/config/reset", s.handleConfigReset)

	// 模型别名映射（独立文件，网关 5s 轮询热生效，无需重启）。
	mux.HandleFunc("GET /api/model-aliases", s.handleAliasesGet)
	mux.HandleFunc("PUT /api/model-aliases", s.handleAliasesPut)
	mux.HandleFunc("POST /api/model-aliases/reset", s.handleAliasesReset)

	// ── 系统 ──────────────────────────────────────────────
	mux.HandleFunc("GET /api/system", s.handleSystem)
	mux.HandleFunc("POST /api/system/restart", s.handleSystemRestart)

	// ── SPA ───────────────────────────────────────────────
	mux.Handle("/", s.static)

	return s.withLogging(s.authMiddleware(mux))
}

// ---------------------------------------------------------------------------
// 总览 / 账号
// ---------------------------------------------------------------------------

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	// 注意：积分补齐不在这里做。所有展示积分的页面（账号页、仪表盘）
	// 都会调 /api/accounts，故统一由 handleAccounts 触发，避免重复。
	writeJSON(w, http.StatusOK, s.svc.Overview(r.Context()))
}

func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	accounts, status, issues, err := s.svc.Accounts(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	// 顺带在后台补齐积分（issue #8）：外部渠道写入的账号在网关池里 credits
	// 初值为 0，而网关只在签到任务里更新它——不主动查就会一直显示 0。
	// 异步执行，不阻塞本响应；前端 20 秒轮询即可看到结果。
	s.svc.EnsureCredits(accounts)
	resp := map[string]any{
		"accounts":    accounts,
		"file_issues": issues,
		"gateway_ok":  status != nil,
	}
	if status != nil {
		resp["summary"] = status
	} else if _, herr := s.svc.Gateway().Health(r.Context()); herr != nil {
		resp["gateway_error"] = herr.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleAccountDetail(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	silent := r.URL.Query().Get("silent") == "true"
	profile, err := s.svc.Profile(r.Context(), uid, silent)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, profile)
}

func (s *Server) handleAccountDelete(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	// 二次确认：删除凭证不可从上游恢复，必须显式带上确认参数。
	if r.URL.Query().Get("confirm") != uid {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "删除账号需确认：请在请求中带上 ?confirm=<uid>",
			"code":  "confirm_required",
		})
		return
	}
	if err := s.svc.DeleteAccount(r.Context(), uid); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "凭证文件已删除，网关将在数秒内将该账号移出账号池（无需重启）"})
}

func (s *Server) handleAccountImport(w http.ResponseWriter, r *http.Request) {
	var in ops.ManualAuthInput
	if !decodeJSON(w, r, &in) {
		return
	}
	res, err := s.svc.SaveManualAuth(in)
	if err != nil {
		writeError(w, err)
		return
	}
	status := http.StatusOK
	if !res.OK {
		status = http.StatusBadRequest
	}
	writeJSON(w, status, res)
}

func (s *Server) handleAccountCheckin(w http.ResponseWriter, r *http.Request) {
	res, err := s.svc.Checkin(r.Context(), r.PathValue("uid"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, statusForResult(res.OK), res)
}

func (s *Server) handleAccountRefresh(w http.ResponseWriter, r *http.Request) {
	res, err := s.svc.RefreshToken(r.Context(), r.PathValue("uid"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, statusForResult(res.OK), res)
}

func (s *Server) handleAccountTravel(w http.ResponseWriter, r *http.Request) {
	res, err := s.svc.Travel(r.Context(), r.PathValue("uid"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, statusForResult(res.OK), res)
}

func (s *Server) handleAccountCredits(w http.ResponseWriter, r *http.Request) {
	c, err := s.svc.CreditsFor(r.Context(), r.PathValue("uid"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// ---------------------------------------------------------------------------
// 批量任务
// ---------------------------------------------------------------------------

// batchRequest 批量操作的目标账号；uids 为空表示全量。
type batchRequest struct {
	UIDs []string `json:"uids"`
}

func (s *Server) decodeBatch(w http.ResponseWriter, r *http.Request) ([]string, bool) {
	var req batchRequest
	// 允许空 body（表示全量）。
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "读取请求体失败"})
		return nil, false
	}
	if len(strings.TrimSpace(string(body))) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误: " + err.Error()})
			return nil, false
		}
	}
	return req.UIDs, true
}

func (s *Server) handleBatchCheckin(w http.ResponseWriter, r *http.Request) {
	uids, ok := s.decodeBatch(w, r)
	if !ok {
		return
	}
	task, err := s.svc.BatchCheckin(r.Context(), uids)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, task)
}

func (s *Server) handleBatchRefresh(w http.ResponseWriter, r *http.Request) {
	uids, ok := s.decodeBatch(w, r)
	if !ok {
		return
	}
	task, err := s.svc.BatchRefresh(r.Context(), uids)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, task)
}

func (s *Server) handleBatchTravel(w http.ResponseWriter, r *http.Request) {
	uids, ok := s.decodeBatch(w, r)
	if !ok {
		return
	}
	task, err := s.svc.BatchTravel(r.Context(), uids)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, task)
}

func (s *Server) handleBatchCredits(w http.ResponseWriter, r *http.Request) {
	uids, ok := s.decodeBatch(w, r)
	if !ok {
		return
	}
	task, err := s.svc.BatchCredits(r.Context(), uids)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, task)
}

func (s *Server) handleTaskList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"tasks":   s.svc.Tasks().Recent(20),
		"running": s.svc.Tasks().Running(),
	})
}

func (s *Server) handleTaskDetail(w http.ResponseWriter, r *http.Request) {
	task, ok := s.svc.Tasks().Get(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "任务不存在或已过期"})
		return
	}
	writeJSON(w, http.StatusOK, task)
}

// ---------------------------------------------------------------------------
// 网页登录
// ---------------------------------------------------------------------------

// loginStartRequest 发起登录的请求：region 决定走国内版还是国际版。
type loginStartRequest struct {
	Region string `json:"region"`
}

func (s *Server) handleLoginStart(w http.ResponseWriter, r *http.Request) {
	// region 允许缺省（默认 cn）；非法值直接拒绝。
	var req loginStartRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		// 允许空 body：region 缺省为 cn。
		req.Region = "cn"
	}
	region, err := upstream.NormalizeRegion(req.Region)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	sess, err := s.svc.StartLogin(region)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

func (s *Server) handleLoginStatus(w http.ResponseWriter, r *http.Request) {
	sess, err := s.svc.Logins().Get(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

func (s *Server) handleLoginPoll(w http.ResponseWriter, r *http.Request) {
	sess, err := s.svc.PollLogin(r.Context(), r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

func (s *Server) handleLoginCancel(w http.ResponseWriter, r *http.Request) {
	sess, err := s.svc.CancelLogin(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

// ---------------------------------------------------------------------------
// 模型 / 聊天
// ---------------------------------------------------------------------------

// statPriceKey 统计行查官方价用的键：**裸名**优先（单价是厂商定价，与 cn/global 无关），
// 老网关不下发 bare 时退回 model 原文。
//
// 网关自统一调度改造后 Model 本身已是裸名，两条路等价；保留回退是为了面板与老版本
// 网关（未带 bare 字段）混跑时不至于整张表查不到价。
func statPriceKey(m gateway.ModelStat) string {
	if m.Bare != "" {
		return m.Bare
	}
	return m.Model
}

// handleStats 返回网关的按模型统计 + 官方价换算。
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	// 时间维度参数透传给网关（range/from/to/interval/model）。
	q := r.URL.Query()
	st, err := s.svc.Gateway().StatsRange(r.Context(), gateway.StatsOptions{
		Range:    q.Get("range"),
		From:     q.Get("from"),
		To:       q.Get("to"),
		Interval: q.Get("interval"),
		Model:    q.Get("model"),
	})
	if err != nil {
		writeError(w, err)
		return
	}
	// 官方价换算：把每个模型的 token 用量折成"走官方 API 要花多少钱"。
	// mode 由前端传（peak/offpeak）—— DeepSeek 空闲价是高峰价的一半。
	mode := pricing.NormalizeTimeMode(r.URL.Query().Get("mode"))
	table := s.svc.Pricing()

	costs := map[string]pricing.Cost{}
	for _, m := range st.Models {
		costs[statPriceKey(m)] = table.Compute(statPriceKey(m), pricing.Usage{
			PromptTokens:     m.PromptTokens,
			CacheHitTokens:   m.CacheHitTokens,
			CacheMissTokens:  m.CacheMissTokens,
			CompletionTokens: m.CompletionTokens,
		}, mode)
	}

	// 汇总必须"各模型分别计价后相加"——不同模型单价不同，
	// 用汇总 token 直接乘单一价格是错的。
	var (
		officialTotal  float64
		cachedCost     float64
		missCost       float64
		outputCost     float64
		pricedModels   []string
		unpricedModels []string
	)
	for _, m := range st.Models {
		c := costs[statPriceKey(m)]
		if !c.Priced {
			unpricedModels = append(unpricedModels, statPriceKey(m))
			continue
		}
		pricedModels = append(pricedModels, statPriceKey(m))
		officialTotal += c.Total
		cachedCost += c.CachedInputCost
		missCost += c.MissInputCost
		outputCost += c.OutputCost
	}

	total := pricing.Cost{
		Model:           "(all)",
		Priced:          true,
		CachedInputCost: cachedCost,
		MissInputCost:   missCost,
		OutputCost:      outputCost,
		Total:           officialTotal,
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"stats":    st,
		"mode":     mode,
		"costs":    costs,
		"total":    total,
		"priced":   pricedModels,
		"unpriced": unpricedModels,
		"pricing": map[string]any{
			"models":     table.ModelsCopy(),
			"source":     table.Source,
			"updated_at": table.UpdatedAt,
			"editable":   s.cfg.PricingFile != "",
		},
	})
}

// handleStatsReset 重置网关统计（写操作，受只读模式约束）。
func (s *Server) handleStatsReset(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.EnsureWritable(); err != nil {
		writeError(w, err)
		return
	}
	if err := s.svc.Gateway().ResetStats(r.Context()); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "网关统计已重置"})
}

// handlePricingUpdate 更新/新增单个模型的官方单价。
//
// 为什么让用户手填而不是预置全部厂商：智谱/Kimi/混元/MiniMax 的定价页是 JS 动态
// 渲染，抓不到权威数字。编造价格会让"省了多少钱"看起来精确但实际是错的，比不做更糟。
func (s *Server) handlePricingUpdate(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.EnsureWritable(); err != nil {
		writeError(w, err)
		return
	}
	if s.cfg.PricingFile == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "服务端未配置价格表路径（pricing_file），无法保存",
			"code":  "not_supported",
		})
		return
	}
	var req struct {
		Model        string  `json:"model"`
		CachedInput  float64 `json:"cached_input"`
		MissInput    float64 `json:"miss_input"`
		Output       float64 `json:"output"`
		OffPeakRatio float64 `json:"off_peak_ratio"`
		Note         string  `json:"note"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "模型名不能为空"})
		return
	}
	if req.CachedInput < 0 || req.MissInput < 0 || req.Output < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "单价不能为负数"})
		return
	}
	s.svc.Pricing().Set(req.Model, pricing.ModelPrice{
		CachedInput:  req.CachedInput,
		MissInput:    req.MissInput,
		Output:       req.Output,
		OffPeakRatio: req.OffPeakRatio,
		Note:         req.Note,
	})
	if err := s.svc.Pricing().Save(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "保存价格表失败：" + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "价格已保存"})
}

// handlePricingDelete 删除单个模型的价格（恢复未配置状态）。
func (s *Server) handlePricingDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.EnsureWritable(); err != nil {
		writeError(w, err)
		return
	}
	s.svc.Pricing().Delete(r.PathValue("model"))
	if err := s.svc.Pricing().Save(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "保存价格表失败：" + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "已移除该模型价格"})
}

// modelView 模型条目 + 按域挂上的促销。内嵌 gateway.Model 让原有字段平铺，
// promotions_cn / promotions_global 是附加字段（该域无促销时省略）。
//
// 为什么按域拆两份而不是一份 promotions：促销是**分域下发**的（PromotionsForRealm
// 各域各取一次上游 /v3/config），同一个模型在国内与国际挂的活动可能完全不同
// （甚至一边免费一边收费）。合成一份必然要在两个域里挑一个，面板另一个 tab 就会
// 显示错域的优惠——此前正是这样：modelRealmBare 只认 "global:"/"cn:" 前缀，
// 而统一调度后目录只剩裸名，于是所有模型都被当成 CN，国际版 tab 恒显示国内活动。
type modelView struct {
	gateway.Model
	PromotionsCN     []upstream.ModelPromotion `json:"promotions_cn,omitempty"`
	PromotionsGlobal []upstream.ModelPromotion `json:"promotions_global,omitempty"`
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	models, err := s.svc.Gateway().Models(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}

	// 促销按域取。best-effort：拿不到只是少一列优惠信息，不该让模型列表打不开，
	// 故失败降级为 promo_errors 里的文案，不影响 data。
	promosByRealm := map[string][]upstream.ModelPromotion{}
	promoErrs := map[string]string{}
	for _, realm := range []string{"global", "cn"} {
		p, msg := s.svc.PromotionsForRealm(realm)
		if len(p) > 0 {
			promosByRealm[realm] = p
		}
		if msg != "" {
			promoErrs[realm] = msg
		}
	}

	out := buildModelViews(models, promosByRealm)

	resp := map[string]any{"data": out, "count": len(out)}
	if len(promoErrs) > 0 {
		resp["promo_errors"] = promoErrs
	}
	writeJSON(w, http.StatusOK, resp)
}

// buildModelViews 给模型目录条目按域挂上促销，产出发给前端的视图。
//
// 抽成纯函数（不碰网络、不碰 svc）是为了能被直接单测——这里正是出过 bug 的地方：
// 旧实现按 id 前缀（"global:"）判断域，而统一调度后目录只剩裸名，于是每个模型都
// 落进 CN 分支，国际版 tab 恒显示国内活动。当时的代码藏在 handleModels 的 HTTP
// 壳里，没有测试够得着，bug 才活了下来。域集合一律取 modelRealms（网关 realms
// 字段优先），别在本函数里再按 id 猜一次。
func buildModelViews(models []gateway.Model, promosByRealm map[string][]upstream.ModelPromotion) []modelView {
	out := make([]modelView, 0, len(models))
	for _, m := range models {
		v := modelView{Model: m}
		bare := modelBare(m.ID)
		for _, realm := range modelRealms(m.ID, m.Realms) {
			switch realm {
			case "global":
				v.PromotionsGlobal = matchRealmPromos(promosByRealm["global"], bare)
			case "cn":
				v.PromotionsCN = matchRealmPromos(promosByRealm["cn"], bare)
			}
		}
		out = append(out, v)
	}
	return out
}

// matchRealmPromos 挑出该域下发、且覆盖该裸名的促销（优先级高的排前面——同域可同时
// 存在「限时免费」与「夜间折扣」两条）。
//
// 只在本域的活动里找：各域活动来自各自 /v3/config（PromotionsForRealm 各取一次），
// 跨域合并会让一个 tab 显示另一个域的活动。模型名按不区分大小写比对（上游大小写
// 不保证与目录一致），与目录侧的裸名对齐靠 modelBare。
func matchRealmPromos(all []upstream.ModelPromotion, bare string) []upstream.ModelPromotion {
	var hit []upstream.ModelPromotion
	for _, p := range all {
		for _, id := range p.ModelIDs {
			if strings.EqualFold(id, bare) {
				hit = append(hit, p)
				break
			}
		}
	}
	sort.SliceStable(hit, func(i, j int) bool { return hit[i].Priority > hit[j].Priority })
	return hit
}

// modelBare 剥掉模型 id 的可选域前缀，得到促销 modelIds 对齐用的裸名。
func modelBare(id string) string {
	if s, ok := strings.CutPrefix(id, "global:"); ok {
		return s
	}
	if s, ok := strings.CutPrefix(id, "cn:"); ok {
		return s
	}
	return id
}

// modelRealms 返回该模型可承接的域集合（顺序 cn, global，与网关 realms 输出一致）。
//
// 首选网关下发的 realms 字段（唯一权威：统一调度后裸名可跨域，前缀已不存在）。
// 缺席（老网关）时按 id 前缀推断：global: → global，其余 → cn，与改造前的口径一致，
// 保证面板对新老两版网关都能正确分域。
func modelRealms(id string, realms []string) []string {
	if len(realms) > 0 {
		out := make([]string, 0, len(realms))
		for _, r := range []string{"cn", "global"} {
			for _, got := range realms {
				if got == r {
					out = append(out, r)
					break
				}
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	if strings.HasPrefix(id, "global:") {
		return []string{"global"}
	}
	return []string{"cn"}
}

// chatRequest 聊天测试台请求。
type chatRequest struct {
	Model    string          `json:"model"`
	Messages []chatMessage   `json:"messages"`
	Stream   bool            `json:"stream"`
	Extra    json.RawMessage `json:"extra,omitempty"` // 透传额外参数（temperature/max_tokens 等）
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// buildChatBody 把测试台请求组装成网关可接受的请求体。
// 关键点：带 conversation_id 以便命中网关的会话粘性，多轮对话不跳号。
func buildChatBody(req chatRequest, conversationID string) ([]byte, error) {
	if strings.TrimSpace(req.Model) == "" {
		return nil, fmt.Errorf("请选择模型")
	}
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("消息不能为空")
	}
	payload := map[string]any{
		"model":    req.Model,
		"messages": req.Messages,
		"stream":   req.Stream,
	}
	// 透传额外参数（仅接受对象形态，避免覆盖关键字段）。
	if len(req.Extra) > 0 {
		var extra map[string]any
		if err := json.Unmarshal(req.Extra, &extra); err == nil {
			for k, v := range extra {
				switch k {
				case "model", "messages", "stream", "conversation_id", "metadata":
					continue // 保留字段不让透传覆盖
				}
				payload[k] = v
			}
		}
	}
	if conversationID != "" {
		payload["conversation_id"] = conversationID
		payload["metadata"] = map[string]any{"conversation_id": conversationID}
	}
	return json.Marshal(payload)
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	body, err := buildChatBody(req, r.Header.Get("X-Conversation-Id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	start := time.Now()
	res, err := s.svc.Gateway().Chat(r.Context(), body)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"result":     res,
		"elapsed_ms": time.Since(start).Milliseconds(),
	})
}

// handleChatStream 流式聊天：把网关 SSE 转成 GUI 自己的 SSE 事件流。
//
// 事件类型：delta（增量文本）/ usage（用量）/ error（错误）/ done（结束）。
func (s *Server) handleChatStream(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Stream = true
	body, err := buildChatBody(req, r.Header.Get("X-Conversation-Id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "当前服务器不支持流式响应"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // 让反代（nginx）不缓冲
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	send := func(v any) {
		raw, err := json.Marshal(v)
		if err != nil {
			return
		}
		_, _ = fmt.Fprintf(w, "data: %s\n\n", raw)
		flusher.Flush()
	}

	start := time.Now()
	var firstTokenAt time.Time
	err = s.svc.Gateway().ChatStream(r.Context(), body, func(d gateway.Delta) error {
		if d.Content != "" && firstTokenAt.IsZero() {
			firstTokenAt = time.Now()
		}
		send(d)
		return nil
	})
	if err != nil {
		send(map[string]any{"error": err.Error()})
	}
	send(map[string]any{
		"done":       true,
		"elapsed_ms": time.Since(start).Milliseconds(),
		"ttfb_ms":    msUntil(firstTokenAt),
	})
}

func msUntil(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return time.Since(t).Milliseconds()
}

// ---------------------------------------------------------------------------
// 网关配置
// ---------------------------------------------------------------------------

func (s *Server) handleConfigGet(w http.ResponseWriter, r *http.Request) {
	doc, meta, err := s.svc.ReadUpstreamConfig()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "meta": meta})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"config": doc, "meta": meta})
}

func (s *Server) handleConfigPut(w http.ResponseWriter, r *http.Request) {
	var doc map[string]any
	if !decodeJSON(w, r, &doc) {
		return
	}
	fallback, err := s.svc.WriteUpstreamConfigDetailed(doc)
	if err != nil {
		writeError(w, err)
		return
	}
	msg := "配置已保存。网关只在启动时读取 config.json，需重启进程才会生效"
	if fallback {
		msg += "。注意：当前 config.json 以 Docker 单文件方式挂载，无法原子替换，本次为原地写入"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"message": msg,
	})
}

func (s *Server) handleConfigReset(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.ResetUpstreamConfig(); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "已从备份恢复初始配置"})
}

// ---------------------------------------------------------------------------
// 模型别名映射
// ---------------------------------------------------------------------------

// handleAliasesGet 返回别名表 + 元信息。
//
// 文件不存在（默认部署就是如此）不算错误：返回空表 + exists=false，
// 让前端从空白表格开始编辑。真正的错误只有两种：推导不出路径（网关未配 state_file）、
// 文件存在但不是合法 JSON（此时 meta.parse_error 有值，前端应提示修正而非覆盖）。
func (s *Server) handleAliasesGet(w http.ResponseWriter, r *http.Request) {
	entries, meta, err := s.svc.ReadAliases()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"aliases": entries, "meta": meta})
}

func (s *Server) handleAliasesPut(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Aliases []ops.AliasEntry `json:"aliases"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	fallback, err := s.svc.WriteAliases(req.Aliases)
	if err != nil {
		writeError(w, err)
		return
	}
	// 提示语与 config.json 刻意不同：本文件热生效。
	msg := fmt.Sprintf("已保存 %d 条别名映射，网关数秒内自动热加载（无需重启）", len(req.Aliases))
	if fallback {
		msg += "。注意：该文件在当前部署下无法原子替换，本次为原地写入"
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": msg})
}

func (s *Server) handleAliasesReset(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.ResetAliases(); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "已从备份恢复别名映射"})
}

// ---------------------------------------------------------------------------
// 系统
// ---------------------------------------------------------------------------

func (s *Server) handleSystem(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	gwHealth, gwErr := s.svc.Gateway().Health(ctx)
	out := map[string]any{
		"version":                s.version,
		"started_at":             s.started,
		"uptime_sec":             int64(time.Since(s.started).Seconds()),
		"read_only":              s.cfg.ReadOnly,
		"dangerous_ops":          s.cfg.DangerousOps,
		"auth_dir":               s.cfg.UpstreamAuthDir,
		"config_file":            s.cfg.UpstreamConfigFile,
		"gateway_url":            s.cfg.GatewayURL,
		"using_default_password": s.cfg.UsingDefaultPassword(),
		"docker_available":       s.svc.DockerAvailable(),
		"container":              s.svc.ContainerStatus(ctx),
	}
	if gwErr != nil {
		out["gateway_health_error"] = gwErr.Error()
	} else {
		out["gateway_health"] = gwHealth
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleSystemRestart(w http.ResponseWriter, r *http.Request) {
	msg, err := s.svc.RestartContainer(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": msg})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError 把业务错误映射为合适的 HTTP 状态码。
func writeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ops.ErrReadOnly):
		writeJSON(w, http.StatusForbidden, map[string]any{"error": err.Error(), "code": "read_only"})
	case errors.Is(err, ops.ErrDangerousDisabled):
		writeJSON(w, http.StatusForbidden, map[string]any{"error": err.Error(), "code": "dangerous_ops_disabled"})
	default:
		msg := err.Error()
		status := http.StatusInternalServerError
		// 账号不存在类错误用 404，其余按 400 处理（都是可向用户展示的原因）。
		if strings.Contains(msg, "账号不存在") || strings.Contains(msg, "不存在") {
			status = http.StatusNotFound
		} else {
			status = http.StatusBadRequest
		}
		writeJSON(w, status, map[string]any{"error": msg})
	}
}

// statusForResult 单账号操作一律返回 200：
// 「操作没成功」是业务结果而非 HTTP 错误，前端按 OpResult.OK 展示具体原因。
// 用 502 之类会让人误以为是面板/网关链路坏了，而实际往往是上游对该账号返回了业务错误。
func statusForResult(_ bool) int {
	return http.StatusOK
}

// decodeJSON 解析请求体；失败时已写好响应，返回 false。
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "读取请求体失败: " + err.Error()})
		return false
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体不能为空"})
		return false
	}
	if err := json.Unmarshal(body, dst); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误: " + err.Error()})
		return false
	}
	return true
}

// statusRecorder 记录响应状态码与字节数，供访问日志使用。
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// Flush 透传 Flusher（SSE 必需）。
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// withLogging 访问日志：记录写操作与慢请求，避免正常读请求刷屏。
func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 静态资源与轮询接口不记日志。
		if !strings.HasPrefix(r.URL.Path, "/api/") ||
			strings.HasPrefix(r.URL.Path, "/api/tasks") ||
			r.URL.Path == "/api/overview" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status >= 400 || r.Method != http.MethodGet {
			log.Printf("%s %s → %d (%s)", r.Method, r.URL.Path, rec.status, time.Since(start).Round(time.Millisecond))
		}
	})
}
