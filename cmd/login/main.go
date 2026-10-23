// main.go — W2A OAuth 登录 CLI（子命令分发 + 单命令交互式流程）。
//
// 子命令：
//
//	login [--realm=cn|global]        → 单命令完成登录（默认；容器内推荐）
//	login [--realm=cn|global] url    → 取授权 URL，state 落盘，stdout 打印 URL
//	login [--realm=cn|global] poll   → 读 state 换 token，stdout 打印完整 JSON
//	login [--realm=cn|global] realm  → 交互式选域，stdout 输出归一化 realm
//	login -h | --help                → 用法
//
// --realm 默认 cn。按 realm 切换上游端点与 Origin/Referer：
//
//	cn     → https://copilot.tencent.com（Origin: https://www.codebuddy.cn）
//	global → https://www.workbuddy.ai（Origin: https://www.workbuddy.ai）
//
// state 落盘带 realm，poll 读回校验与命令行 --realm 一致（防混域）。
// 无 PKCE（workbuddy 设备流由服务端签发 state）。
//
// 单命令模式（不带子命令）的设计前提：宿主机不必装 Go / python3，整个流程在容器内
// 一次跑完（state 只存活于进程内存，不落盘，因此 `--rm` 不会丢）：
//
//	docker run --rm -it -v "$PWD/auths:/app/auths" --user root \
//	  --entrypoint /app/login ghcr.io/sofs2005/w2a:latest
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// appUID 镜像内服务进程 uid（Dockerfile: adduser -u 10001 app）。
// 单命令模式以 root 落盘时把产物 chown 给它，保证服务容器可读写 auths（token 续期要回写）。
const appUID = 10001

// 登录 state 落盘路径（var 便于测试替换临时文件；仅 url/poll 两段式使用）。
// 跨平台：os.TempDir() 在 Linux 解析为 /tmp（容器内行为不变），Windows 解析为
// 系统临时目录，避免硬编码 /tmp 在 Windows 上 "The system cannot find the path"。
// 注：上游常量（upstreamBaseCN / clientUA / originReferer* 等）在 oauth.go —— 本包
// 把「跟上游说话」的 HTTP 层收在 oauth.go，main.go 只留 CLI 层（含 realmConfig 用的
// base/origin 选择）。两文件同包，常量只声明一处。
var stateFile = filepath.Join(os.TempDir(), "wb2api-login-state.json")

// exitFunc 供测试替换（默认 os.Exit；测试持临时替换为 panic 以进程内捕获 fatal）。
var exitFunc = os.Exit

// realm 取值枚举（与 internal/auth 的 Realm() 归一化输出一致）。
const (
	realmCN     = "cn"
	realmGlobal = "global"
)

// usage 帮助文本（-h / --help / help）。
const usage = `W2A OAuth 登录

用法:
  login [--realm=cn|global]          单命令完成登录（默认，容器内推荐）
  login [--realm=cn|global] url      取授权 URL（state 落盘，供 poll 使用）
  login [--realm=cn|global] poll     用 state 换 token，stdout 输出 JSON
  login [--realm=cn|global] realm    交互式选域，stdout 输出 cn|global
  login -h | --help                  显示本帮助

环境变量:
  AUTH_DIR    单命令模式落盘目录（默认 /app/auths）
  CHOWN_UID   单命令模式产物归属 uid（默认 10001；0 表示不 chown）

--realm 默认 cn；global 走 www.workbuddy.ai（国际版）。
`

// loginState url 子命令落盘的 state（带 realm，poll 读回校验防混域）。
type loginState struct {
	State string `json:"state"`
	Realm string `json:"realm,omitempty"`
}

// realmConfig 按 realm 返回上游 base 与 Origin/Referer origin：global →
// (www.workbuddy.ai, www.workbuddy.ai)；cn/非法/缺省 → (copilot.tencent.com, codebuddy.cn)。
func realmConfig(realm string) (base, origin string) {
	if realm == realmGlobal {
		return upstreamBaseGlobal, originRefererGlobal
	}
	return upstreamBaseCN, originRefererCN
}

// commonHeaders / apiEnvelope / doJSON 见 oauth.go（本包的 HTTP 端点层）。
// main.go 只保留 CLI 层，避免同名符号两处声明。

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "login: "+format+"\n", args...)
	exitFunc(1)
}

// parseRealmArgs 解析开头的 --realm=cn|global（或分离式 --realm <v>）flag，缺省 cn。
// 大小写不敏感归一化；非法值/缺值报错。剥离 flag 后剩余参数（子命令）顺序不变。
func parseRealmArgs(args []string) (realm string, rest []string, err error) {
	realm = realmCN
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--realm":
			if i+1 >= len(args) {
				return "", nil, fmt.Errorf("--realm requires a value")
			}
			v := strings.ToLower(strings.TrimSpace(args[i+1]))
			if v != realmCN && v != realmGlobal {
				return "", nil, fmt.Errorf("invalid --realm %q (want cn|global)", args[i+1])
			}
			realm = v
			i++
		case strings.HasPrefix(a, "--realm="):
			v := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(a, "--realm=")))
			if v != realmCN && v != realmGlobal {
				return "", nil, fmt.Errorf("invalid --realm %q (want cn|global)", v)
			}
			realm = v
		default:
			rest = append(rest, a)
		}
	}
	return realm, rest, nil
}

// resolveRealmInput 把交互式选域的一行输入归一化为 realm（纯函数，login.sh 交互分支
// 的核心决策，可测）。规则：
//
//	"1"/"cn"（大小写不敏感）/""（回车默认）→ cn
//	"2"/"global" → global
//	其他 → ("", false)（调用方回默认 cn）
func resolveRealmInput(input string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(input)) {
	case "", "1", "cn":
		return realmCN, true
	case "2", "global":
		return realmGlobal, true
	}
	return "", false
}

// promptRealm 交互式选域：向 out 打印选项提示（out 接 stderr，stdout 留给 realm 本身），
// 从 in 读一行，返回归一化 realm。非法输入警告后回落 cn；EOF（非交互/管道）回落 cn。
func promptRealm(in io.Reader, out io.Writer) string {
	fmt.Fprintln(out, "选择登录版本: 1) 国内版(cn) 2) 国际版(global) [默认 1/cn]: ")
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		// EOF/非交互 → 回落默认 cn
		return realmCN
	}
	if realm, ok := resolveRealmInput(line); ok {
		return realm
	}
	fmt.Fprintln(out, "无效选择，默认国内版 cn")
	return realmCN
}

// validateRealmMatch 校验 state 文件 realm 与命令行 --realm 一致（防混域）：
// state 无 realm（旧文件）放行；非空且不一致 → error。
func validateRealmMatch(stateRealm, cliRealm string) error {
	if stateRealm != "" && stateRealm != cliRealm {
		return fmt.Errorf("realm mismatch: state file realm=%q, command --realm=%q（url 与 poll 需同一 realm）", stateRealm, cliRealm)
	}
	return nil
}

// runURL 执行 url 子命令：向 base 的 state 端点 POST 取授权 URL，
// state 落盘（带 realm），stdout 打印 authURL。out 接 stdout；statePath 为落盘路径
// （可注入临时文件便于测试）。
func runURL(base, origin, realm, statePath string, client *http.Client, out io.Writer) {
	authURL, state, err := fetchAuthURL(client, base, origin)
	if err != nil {
		fatal("%v", err)
	}
	raw, _ := json.Marshal(loginState{State: state, Realm: realm})
	if err := os.WriteFile(statePath, raw, 0o600); err != nil {
		fatal("write state: %v", err)
	}
	fmt.Fprintln(out, authURL)
}

// runPoll 执行 poll 子命令：读 state 文件（realm 校验），向 base 的 token 端点
// GET 一次，成功再 GET login/account（带 Bearer），stdout 打印完整 token+account JSON。
// statePath 可注入临时文件便于测试。
func runPoll(base, origin, realm, statePath string, client *http.Client, out io.Writer) {
	raw, err := os.ReadFile(statePath)
	if err != nil {
		fatal("read state: %v (先跑 login url)", err)
	}
	var ls loginState
	if err := json.Unmarshal(raw, &ls); err != nil {
		fatal("parse state: %v", err)
	}
	// 防混域：state 落盘 realm 与命令行 --realm 不一致则拒绝（url 与 poll 必须同域）
	if err := validateRealmMatch(ls.Realm, realm); err != nil {
		fatal("%v", err)
	}
	// fetchToken 即上游内联的 handlePollLogin 逻辑（auth/token 权威登录状态端点 +
	// login/account 取 uid），已抽到 oauth.go 供 runPoll / runOnce 共用。
	tb, err := fetchToken(client, base, origin, ls.State)
	if err != nil {
		fatal("%v", err)
	}
	oraw, _ := json.Marshal(buildLoginOutput(tb.tokenFields(), realm, tb.accountFields()))
	fmt.Fprintln(out, string(oraw))
	os.Remove(statePath)
}

// buildLoginOutput 组装 poll 输出的完整 JSON（login.sh 据此落盘 auth 文件）。
// realm 永不空：显式 --realm 优先（ResolveRealm 处理），否则按上游返回的 domain 推断——
// 保证登录落盘的 auth 文件恒带 realm 键。
func buildLoginOutput(tok struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresIn    int64  `json:"expiresIn"`
	Domain       string `json:"domain"`
}, realm string, acct struct {
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
}) map[string]any {
	return map[string]any{
		"access_token":  tok.AccessToken,
		"refresh_token": tok.RefreshToken,
		"expires_in":    tok.ExpiresIn,
		"domain":        tok.Domain,
		"realm":         auth.ResolveRealm(realm, tok.Domain),
		"uid":           acct.UID,
		"enterprise_id": acct.EnterpriseID,
		"nickname":      acct.Nickname,
	}
}

// runOnce 单命令完成整个登录流程（容器内推荐用法，`--entrypoint /app/login` 无参数即触发）：
// 取授权 URL → 等用户在浏览器完成登录 → 按 y → 换 token → 首次签到 → 落盘 auth 文件。
//
// 与 url/poll 两段式的区别：state 只存活于本进程内存，不落盘，因此 `docker run --rm`
// 不会丢 state，宿主机也无需 Go / python3。落盘复用 internal/auth（原子写 + realm 标识），
// 以 root 运行时把产物 chown 给服务进程 uid（uid 来自 CHOWN_UID，默认 appUID）。
func runOnce(base, origin, realm string, client *http.Client) {
	authDir := envOr("AUTH_DIR", "/app/auths")
	chownUID := appUID
	if v := os.Getenv("CHOWN_UID"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			chownUID = n
		}
	}
	out := os.Stdout
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		fatal("创建 auth 目录: %v", err)
	}

	authURL, state, err := fetchAuthURL(client, base, origin)
	if err != nil {
		fatal("%v", err)
	}

	fmt.Fprintln(out, "============================================================")
	fmt.Fprintln(out, "  W2A OAuth 登录")
	fmt.Fprintln(out, "============================================================")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "请在浏览器中打开以下链接完成登录：")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "  "+authURL)
	fmt.Fprintln(out)

	// 容器内无剪贴板，直接等用户在浏览器登录后回来确认
	fmt.Fprint(out, "完成登录后按 y 继续: ")
	ans, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		fatal("读取输入失败（需要 -it 交互模式）: %v", err)
	}
	ans = strings.ToLower(strings.TrimSpace(ans))
	if ans != "y" && ans != "yes" {
		fatal("已取消")
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, "正在获取 token...")

	tb, err := fetchToken(client, base, origin, state)
	if err != nil {
		fmt.Fprintln(os.Stderr, "获取 token 失败。可能原因：")
		fmt.Fprintln(os.Stderr, "  - 登录还没完成就按了 y（重新运行再试）")
		fmt.Fprintln(os.Stderr, "  - 登录页报错（把报错截图发出来排查）")
		fatal("%v", err)
	}

	// ─── 签到（失败不阻断登录）───────────────────────────────────
	doCheckin(tb, realm, out)

	// ─── 落盘 auth 文件（与 internal/auth 读取格式一致）─────────────
	a := &auth.Auth{
		AccessToken:  tb.AccessToken,
		RefreshToken: tb.RefreshToken,
		ExpiresAt:    time.Now().Unix() + tb.ExpiresIn,
		Domain:       tb.Domain,
		UID:          tb.UID,
		EnterpriseID: tb.EnterpriseID,
		Nickname:     tb.Nickname,
	}
	// 显式 --realm 优先于 domain 推断（与 buildLoginOutput 的 ResolveRealm 口径一致）
	a.SetRealm(realm)
	file := filepath.Join(authDir, "workbuddy-"+tb.UID+".json")
	a.FilePath = file

	// 重新登录是**整份覆盖**（SaveAtomic 从结构体重建文档），故先读回旧文件里
	// 手写的账号级 proxy / device_token 一并带上——否则「重新登录一次」就把它们
	// 悄悄抹掉了（面板侧的 preserve.go 只护住面板自己的保存路径，护不到这里）。
	carriedProxy := carryPreservedFields(a, file)

	if _, statErr := os.Stat(file); statErr == nil {
		carried := ""
		if carriedProxy {
			carried = "（已保留原有账号级代理配置）"
		}
		fmt.Fprintf(out, "账号已存在（uid=%s），将覆盖更新凭证%s\n", tb.UID, carried)
	} else {
		fmt.Fprintf(out, "新账号（uid=%s），新增 auth 文件\n", tb.UID)
	}

	if err := a.SaveAtomic(); err != nil {
		fatal("写入 auth 文件: %v", err)
	}
	// 服务端 refresh 后要回写文件，权限必须是服务进程可读可写
	if err := os.Chmod(file, 0o600); err != nil {
		fatal("chmod auth 文件: %v", err)
	}
	if err := chownToService(file, authDir, chownUID, out); err != nil {
		fatal("%v", err)
	}

	fmt.Fprintf(out, "已保存: %s\n", file)
	fmt.Fprintln(out)
	fmt.Fprintln(out, "============================================================")
	fmt.Fprintln(out, "  登录完成！")
	fmt.Fprintln(out, "  UID:      "+tb.UID)
	fmt.Fprintln(out, "  Realm:    "+a.Realm())
	fmt.Fprintln(out, "  Nickname: "+displayOr(tb.Nickname, "（未获取到）"))
	fmt.Fprintln(out, "  Token:    "+truncate(tb.AccessToken, 30)+"...")
	fmt.Fprintln(out, "  有效期:   "+time.Unix(a.ExpiresAt, 0).Format("2006-01-02 15:04"))
	fmt.Fprintln(out, "============================================================")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "重启服务加载新账号：docker compose restart")
}

// carryPreservedFields 把已存在 auth 文件里**本流程不产生**的字段带进 a，供随后的
// SaveAtomic 整份覆盖时不丢：账号级 proxy（手写配置，本期无调度逻辑）与 device_token
// （容器内无 Turing SDK，由宿主注入，丢了不可反推）。
//
// 返回是否带上了账号级代理（供提示语措辞）。文件不存在 / 不可读 / 解析失败都静默跳过
// 并返回 false：重新登录本身可能就是在修一个损坏的文件，此时不该被旧内容挡住。
func carryPreservedFields(a *auth.Auth, file string) bool {
	raw, err := os.ReadFile(file)
	if err != nil {
		return false
	}
	old, err := auth.Parse(raw)
	if err != nil {
		return false
	}
	a.ProxyName = old.ProxyName
	a.DeviceToken = old.DeviceToken
	return old.ProxyName != ""
}

// doCheckin 首次签到（幂等）。失败仅提示，不阻断——调度器每日 09:00/21:00 会重试。
// global realm 跳过：与调度器 D4 门控一致（国际版签到端点未实测，避免误打 CN 端点）。
func doCheckin(tb tokenBundle, realm string, out io.Writer) {
	if realm == realmGlobal {
		fmt.Fprintln(out, "签到: global realm 跳过（国际版签到端点未实测）")
		return
	}
	a := &auth.Auth{
		AccessToken:  tb.AccessToken,
		RefreshToken: tb.RefreshToken,
		Domain:       tb.Domain,
		UID:          tb.UID,
		EnterpriseID: tb.EnterpriseID,
		Nickname:     tb.Nickname,
	}
	a.SetRealm(realm)
	// 账号级出口线路：新登录的号若已有线路绑定（上面 carryPreservedFields 带过来的），
	// 首次签到也应经该线路出网。
	upstream.LoadRouteTable("")
	if err := upstream.New().DailyCheckin(a); err != nil {
		fmt.Fprintf(out, "签到: 失败（%v）—— 调度器会于 09:00/21:00 重试\n", err)
		return
	}
	fmt.Fprintln(out, "签到: 成功")
}

// chownToService 以 root 运行时把产物归属改给服务进程，避免宿主目录属主
// 与服务容器 uid 不一致导致 token 续期回写失败。
func chownToService(file, dir string, uid int, out io.Writer) error {
	if uid <= 0 || os.Geteuid() != 0 {
		return nil
	}
	if err := os.Chown(dir, uid, uid); err != nil {
		return fmt.Errorf("chown auth 目录: %w", err)
	}
	if err := os.Chown(file, uid, uid); err != nil {
		return fmt.Errorf("chown auth 文件: %w", err)
	}
	fmt.Fprintf(out, "已将 %s 归属改为 uid=%s（服务进程）\n", dir, strconv.Itoa(uid))
	return nil
}

func main() {
	realm, rest, err := parseRealmArgs(os.Args[1:])
	if err != nil {
		fatal("%v (usage: login [--realm=cn|global] [url|poll|realm])", err)
	}
	// -h/--help/help：打印用法并正常退出（CI 冒烟检查依赖退出码 0）
	if len(rest) > 0 {
		switch rest[0] {
		case "-h", "--help", "help":
			fmt.Print(usage)
			return
		}
	}
	base, origin := realmConfig(realm)
	// newLoginClient（oauth.go）= 上游此处的 cookiejar + 30s 超时，每个流程独立 jar
	// （多账号登录互不串会话）。代理按 realm 取自 config.json 的 proxy 段（env
	// WB2A_PROXY_<REALM> 优先）——国内用户首次登录 global 账号时网关可能还没起，
	// 配置文件里的代理是此时唯一可达路径。
	proxyURL := proxyForRealm(realm, envOr("CONFIG_FILE", "./config.json"))
	client, err := newLoginClient(proxyURL)
	if err != nil {
		// 代理配置错就该立刻报错：登录是交互式工具，静默回落直连会让用户
		// 对着「连不上」白等一轮，而根因（URL 写错）其实一眼可见。
		fatal("代理配置（proxy.%s）无效: %v", realm, err)
	}
	if proxyURL != "" {
		// 用脱敏描述而非原始 URL：代理串里内嵌账号密码，而这一行会进终端回滚、
		// 被截图、被贴进 issue。scheme+host 已足够确认「是不是我设的那个代理」。
		// 解析失败不会走到这里（上面 fatal 已拦），故忽略错误。
		if u, perr := upstream.ParseProxyURL(proxyURL); perr == nil && u != nil {
			fmt.Fprintf(os.Stderr, "（经代理出站: %s）\n", upstream.ProxyDesc(u))
		}
	}

	// 无子命令 = 单命令登录（容器内 `--entrypoint /app/login` 无参数直接可用）
	if len(rest) < 1 {
		runOnce(base, origin, realm, client)
		return
	}

	switch rest[0] {
	case "url":
		runURL(base, origin, realm, stateFile, client, os.Stdout)

	case "poll":
		runPoll(base, origin, realm, stateFile, client, os.Stdout)

	case "realm":
		// 交互式选域（login.sh 无 --realm 传参且 stdin 为 tty 时调用）。
		// 提示打到 stderr，stdout 只输出归一化 realm，供 $( ) 捕获。
		fmt.Println(promptRealm(os.Stdin, os.Stderr))

	case "once":
		runOnce(base, origin, realm, client)

	default:
		fatal("unknown subcommand %q (want url|poll|realm)", rest[0])
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func displayOr(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
