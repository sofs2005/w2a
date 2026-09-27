// session.go 面板自身的登录会话：内存态 token + HttpOnly Cookie。
//
// 为什么必须有鉴权：GUI 能读到账号 accessToken / refreshToken，等同账号完全控制权，
// 且支持改配置、重启容器。默认口令也强制存在，不允许无鉴权直通（见 config.UIConfig 注释）。
package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"workbuddy2api-gui/internal/config"
)

const (
	sessionCookieName = "wbgui_session"

	// maxLoginFailures / loginLockWindow 暴力破解防护：同一来源连续失败达上限后锁定一段时间。
	maxLoginFailures = 10
	loginLockWindow  = 10 * time.Minute
)

// session 一条已登录会话。
type session struct {
	token     string
	username  string
	expiresAt time.Time
}

// loginAttempt 单个来源的失败计数。
type loginAttempt struct {
	failures int
	firstAt  time.Time
}

// SessionStore 内存会话表。
type SessionStore struct {
	mu       sync.Mutex
	sessions map[string]session
	attempts map[string]loginAttempt
	ttl      time.Duration
}

// NewSessionStore 构建会话表。
func NewSessionStore(ttl time.Duration) *SessionStore {
	if ttl <= 0 {
		ttl = 12 * time.Hour
	}
	return &SessionStore{
		sessions: map[string]session{},
		attempts: map[string]loginAttempt{},
		ttl:      ttl,
	}
}

// Create 校验口令并创建会话；成功返回 token。
func (s *SessionStore) Create(username, password string, cfg *config.Config, source string) (string, bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 先做锁定检查。
	if a, ok := s.attempts[source]; ok {
		if a.failures >= maxLoginFailures && time.Since(a.firstAt) < loginLockWindow {
			remain := (loginLockWindow - time.Since(a.firstAt)).Round(time.Second)
			return "", false, "失败次数过多，请 " + remain.String() + " 后再试"
		}
		if time.Since(a.firstAt) >= loginLockWindow {
			delete(s.attempts, source)
		}
	}

	// 常量时间比较，避免时序侧信道泄露口令长度/内容。
	userOK := subtle.ConstantTimeCompare([]byte(username), []byte(cfg.UI.Username)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(password), []byte(cfg.UI.Password)) == 1
	if !userOK || !passOK {
		att := s.attempts[source]
		if att.failures == 0 || time.Since(att.firstAt) >= loginLockWindow {
			att = loginAttempt{firstAt: time.Now()}
		}
		att.failures++
		s.attempts[source] = att
		return "", false, "用户名或密码错误"
	}

	delete(s.attempts, source) // 成功即清空失败计数

	token, err := newToken()
	if err != nil {
		return "", false, "生成会话失败"
	}
	s.sessions[token] = session{token: token, username: username, expiresAt: time.Now().Add(s.ttl)}
	s.gcLocked()
	return token, true, ""
}

// Validate 校验 token 是否有效，返回用户名。
func (s *SessionStore) Validate(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[token]
	if !ok {
		return "", false
	}
	if time.Now().After(sess.expiresAt) {
		delete(s.sessions, token)
		return "", false
	}
	return sess.username, true
}

// Revoke 注销会话。
func (s *SessionStore) Revoke(token string) {
	s.mu.Lock()
	delete(s.sessions, token)
	s.mu.Unlock()
}

// RevokeAll 吊销全部会话（改密码后旧会话全部失效）。
func (s *SessionStore) RevokeAll() {
	s.mu.Lock()
	s.sessions = map[string]session{}
	s.mu.Unlock()
}

// gcLocked 清理过期会话。调用方必须已持锁。
func (s *SessionStore) gcLocked() {
	now := time.Now()
	for t, sess := range s.sessions {
		if now.After(sess.expiresAt) {
			delete(s.sessions, t)
		}
	}
}

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// clientSource 取来源标识（IP），用于失败计数。
func clientSource(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// ---------------------------------------------------------------------------
// 中间件
// ---------------------------------------------------------------------------

// authMiddleware 校验面板会话。放行：/api/login、/api/session、静态资源。
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 只保护 /api/*；静态资源与 SPA 入口放行（前端首屏自行跳登录页）。
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		switch r.URL.Path {
		case "/api/login", "/api/session":
			next.ServeHTTP(w, r)
			return
		}
		if _, ok := s.currentUser(r); !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": "未登录或会话已过期",
				"code":  "unauthorized",
			})
			return
		}
		// 写操作的 CSRF 防线：带 Cookie 的请求要求同源。
		// SameSite=Strict 已挡住绝大多数跨站请求，这里再校验 Origin 作为纵深防御。
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if origin := r.Header.Get("Origin"); origin != "" {
				if !sameOrigin(origin, requestHosts(r), s.cfg.AllowedOrigins) {
					writeJSON(w, http.StatusForbidden, map[string]any{
						"error": "跨站请求被拒绝。若本面板部署在反向代理之后，" +
							"请确认代理透传了 Host 或 X-Forwarded-Host 头。",
						"code": "csrf",
					})
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// currentUser 从 Cookie 或 Authorization 头解析当前用户。
// 支持 Bearer 是为了让脚本 / curl 也能调用 API（自动化场景）。
func (s *Server) currentUser(r *http.Request) (string, bool) {
	if c, err := r.Cookie(sessionCookieName); err == nil {
		if user, ok := s.sessions.Validate(c.Value); ok {
			return user, true
		}
	}
	if authz := r.Header.Get("Authorization"); strings.HasPrefix(authz, "Bearer ") {
		if user, ok := s.sessions.Validate(strings.TrimPrefix(authz, "Bearer ")); ok {
			return user, true
		}
	}
	return "", false
}

// requestHosts 收集本次请求「可能被浏览器用作 Host」的所有候选值。
//
// 为什么需要一个列表：反向代理下 r.Host 未必等于浏览器地址栏里的主机名——
//
//   - nginx 默认 proxy_set_header Host $host 会**丢掉端口**（用户访问 :8787，
//     后端看到 panel.example.com）；
//   - 配了 proxy_set_header Host $proxy_host 或漏配时，后端看到的是
//     127.0.0.1:8787 这种内网地址；
//   - 透传 Host 时可能带上端口而 Origin 不带（或反之）。
//
// 因此把 Host 与各 X-Forwarded-* / Forwarded 都作为候选，任一匹配即视为同源。
//
// 安全性说明：攻击者能伪造这些头，但**浏览器不允许**脚本设置 Host /
// X-Forwarded-Host（它们属于禁止修改的头，或会被代理解析覆盖），因此对
// 跨站攻击者来说这些候选值他控制不了——他能控的只有 Origin，而 Origin 由
// 浏览器写入、无法伪造。真正的信任边界仍是「代理透传了什么」，这与
// X-Forwarded-For 的既有前提一致。
func requestHosts(r *http.Request) []string {
	hosts := []string{r.Host}
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v != "" {
			hosts = append(hosts, v)
		}
	}
	// X-Forwarded-Host 可能是逗号分隔的链（proxy1, proxy2），逐个取。
	for _, raw := range r.Header.Values("X-Forwarded-Host") {
		for _, part := range strings.Split(raw, ",") {
			add(part)
		}
	}
	// RFC 7239 Forwarded: for=1.2.3.4;host=example.com;proto=https（可能多条）。
	for _, raw := range r.Header.Values("Forwarded") {
		for _, elem := range strings.Split(raw, ",") {
			for _, kv := range strings.Split(elem, ";") {
				kv = strings.TrimSpace(kv)
				i := strings.IndexByte(kv, '=')
				if i < 0 || !strings.EqualFold(strings.TrimSpace(kv[:i]), "host") {
					continue
				}
				add(strings.Trim(strings.TrimSpace(kv[i+1:]), `"`))
			}
		}
	}
	// X-Forwarded-Port 补端口：某些代理只透传 host 与 port 分开的两个头。
	if p := strings.TrimSpace(r.Header.Get("X-Forwarded-Port")); p != "" {
		for _, h := range append([]string(nil), hosts...) {
			if !strings.Contains(h, ":") {
				hosts = append(hosts, h+":"+p)
			}
		}
	}
	return hosts
}

// sameOrigin 报告 Origin 是否与候选主机之一同源。
//
// 比较口径是**主机名**（忽略协议与端口）：
//   - 忽略协议：反代下后端是 http、用户走 https，协议天然不同，比较协议会
//     把所有反代部署判成跨站（这正是 issue #7）。
//   - 忽略端口：nginx 默认丢端口、或用户经 443/80 默认端口访问时 Origin 不带
//     端口而后端 Host 带，比较端口同样会误判。
//
// 放宽端口是否会削弱 CSRF 防护？不会：CSRF 的攻击面是「另一个站点冒充本面板」，
// 站点由**主机名**区分。同一主机名的不同端口属于同一站点的不同服务，浏览器
// 的同源策略本就按「协议+主机+端口」三元组划分，但我们的威胁模型是"跨站点
// 伪造请求"，同主机名的其他端口并不构成跨站攻击面（且攻击者要在你的主机名
// 上另起端口，已属于另一层面的失陷）。宁可放宽端口，也不能让正常反代部署不可用。
//
// 空 Origin、"null"（沙箱 iframe / file://）等一律拒绝。
//
// extra 为用户显式配置的额外白名单（config.allowed_origins），用于反代连
// Host 都不透传、后端无从推导的兜底场景。
func sameOrigin(origin string, hosts []string, extra []string) bool {
	oh := hostnameOf(origin)
	if oh == "" {
		return false
	}
	for _, h := range hosts {
		if hh := hostnameOf(h); hh != "" && strings.EqualFold(hh, oh) {
			return true
		}
	}
	for _, e := range extra {
		if eh := hostnameOf(e); eh != "" && strings.EqualFold(eh, oh) {
			return true
		}
	}
	return false
}

// hostnameOf 从 Origin/URL/Host 形式的字符串里取出裸主机名（小写、无端口、无协议）。
// 无法解析时返回空串。
func hostnameOf(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// 去掉协议前缀。
	for _, scheme := range []string{"http://", "https://"} {
		s = strings.TrimPrefix(s, scheme)
		s = strings.TrimPrefix(s, scheme) // 容错重复前缀
	}
	// 去掉路径/查询（Origin 不带，但 Forwarded 的 host 参数可能被写错）。
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	// 去掉 userinfo。
	if i := strings.LastIndexByte(s, '@'); i >= 0 {
		s = s[i+1:]
	}
	// 去掉端口。注意 IPv6 字面量形如 [::1]:8787，需按 ] 判断。
	if strings.HasPrefix(s, "[") {
		if i := strings.IndexByte(s, ']'); i >= 0 {
			s = s[:i+1]
		}
	} else if i := strings.LastIndexByte(s, ':'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSuffix(s, ".")
	return strings.ToLower(s)
}

// ---------------------------------------------------------------------------
// 登录接口
// ---------------------------------------------------------------------------

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// handleLogin 面板登录。
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	token, ok, msg := s.sessions.Create(req.Username, req.Password, s.cfg, clientSource(r))
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": msg})
		return
	}
	// SameSite=Strict 阻断跨站携带；Secure 交由部署方按 HTTPS 情况决定（本地 http 无法设 Secure）。
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(s.sessions.ttl.Seconds()),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"token":    token,
		"username": req.Username,
	})
}

// handleLogout 注销。
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookieName); err == nil {
		s.sessions.Revoke(c.Value)
	}
	if authz := r.Header.Get("Authorization"); strings.HasPrefix(authz, "Bearer ") {
		s.sessions.Revoke(strings.TrimPrefix(authz, "Bearer "))
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: "", Path: "/", HttpOnly: true, MaxAge: -1,
		SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleSessionInfo 返回当前会话与运行模式信息（前端首屏用，无需登录即可调用，
// 但未登录时不返回任何敏感信息）。
func (s *Server) handleSessionInfo(w http.ResponseWriter, r *http.Request) {
	user, authed := s.currentUser(r)
	resp := map[string]any{
		"authenticated":          authed,
		"username":               user,
		"read_only":              s.cfg.ReadOnly,
		"dangerous_ops":          s.cfg.DangerousOps,
		"using_default_password": s.cfg.UsingDefaultPassword(),
		"gateway_url":            s.cfg.GatewayURL,
		"password_changeable":    s.cfg.CredentialsFile != "",
	}
	if !authed {
		// 未登录也透露「是否仍在用默认口令」是必要的：否则用户不知道去哪改。
		// 这里不含任何账号信息，风险可接受。
		resp["username"] = ""
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// 改密码
// ---------------------------------------------------------------------------

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
	NewUsername     string `json:"new_username"`
}

// handleChangePassword 修改面板登录口令（可选同时改用户名）。
//
// 安全要求：
//  1. 必须已登录（authMiddleware 已保证）。
//  2. 必须提供正确的当前口令（防止他人用遗留会话改密码）。
//  3. 新口令非空；未提供新用户名则沿用现有用户名。
//  4. 凭据持久化到 credentials_file（若配置了）供重启后生效。
//  5. 改成功后吊销所有会话、让客户端重新登录（旧口令立即失效）。
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	if s.cfg.CredentialsFile == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "服务端未配置凭据持久化路径（credentials_file），无法保存新密码",
			"code":  "not_supported",
		})
		return
	}

	var req changePasswordRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}

	// 校验当前口令（常量时间比较）。
	if subtle.ConstantTimeCompare([]byte(req.CurrentPassword), []byte(s.cfg.UI.Password)) != 1 {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error": "当前口令不正确",
			"code":  "wrong_password",
		})
		return
	}
	if req.NewPassword == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "新口令不能为空"})
		return
	}
	if len(req.NewPassword) < 6 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "新口令至少 6 位"})
		return
	}

	newUsername := s.cfg.UI.Username
	if req.NewUsername != "" {
		newUsername = req.NewUsername
	}

	// 先落盘：持久化成功才更新内存，避免「内存改了但重启回退」的不一致。
	if err := s.cfg.SaveStoredCredentials(newUsername, req.NewPassword); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "保存新口令失败：" + err.Error()})
		return
	}

	// 生效：更新内存配置 + 吊销所有会话（让旧口令与旧会话都立即失效）。
	s.cfg.UI.Username = newUsername
	s.cfg.UI.Password = req.NewPassword
	s.sessions.RevokeAll()

	// 用新口令重新给当前请求者发一个会话（无缝续期，不必再输一次）。
	token, ok, msg := s.sessions.Create(newUsername, req.NewPassword, s.cfg, clientSource(r))
	if !ok {
		// 理论上不会失败（刚校验过），失败时让前端跳登录页即可。
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":       true,
			"username": newUsername,
			"message":  "口令已修改，请重新登录",
			"relogin":  true,
			"error":    msg,
		})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(s.sessions.ttl.Seconds()),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"username": newUsername,
		"message":  "口令已修改并保存，重启面板后依然生效",
	})
}
