// Package host 把面板装配成一个 http.Handler，交由宿主在同一监听上挂载。
//
// 为什么需要这个包（而不是让宿主直接装配）：
// Go 的 internal 可见性规则规定 `workbuddy2api-gui/internal/*` 只能被
// `workbuddy2api-gui/` 树内的代码 import。合并后宿主是另一个 module
// （workbuddy2api），无论怎么 replace 都跨不过这条规则。因此装配必须发生在本
// module 内部，而本包刻意**不含 internal 段**，宿主才能 import 它。
//
// 本包是合并时新增的（上游面板没有），因此今后 git subtree pull 不会与它冲突。
package host

import (
	"fmt"
	"log"
	"net/http"
	"path/filepath"

	panelapi "workbuddy2api-gui/internal/api"
	panelstore "workbuddy2api-gui/internal/authstore"
	panelconfig "workbuddy2api-gui/internal/config"
	panelgateway "workbuddy2api-gui/internal/gateway"
	panelops "workbuddy2api-gui/internal/ops"
	panelupstream "workbuddy2api-gui/internal/upstream"
	panelwebui "workbuddy2api-gui/internal/webui"
)

// Options 宿主注入的网关侧上下文。
//
// 面板配置一律**只认 WBGUI_* 环境变量**（见 config.Load("")）：合并后同一进程里
// 有两份 config.json 的语义，读文件极易把网关那份当面板配置读进来（面板自带的
// rejectGatewayConfig 正是为这个历史坑加的防呆）。这里只覆盖必须由宿主提供的项。
type Options struct {
	// GatewayURL 面板访问网关的地址。同进程部署应传回环地址
	// （如 http://127.0.0.1:7863），面板据此调 /status、/v1/models 与聊天台。
	GatewayURL string
	// GatewayAPIKey 网关的 api_key；留空时 /status 会 401。
	GatewayAPIKey string
	// AuthDir 网关账号凭证目录（auths/）。面板直读直写，用于账号页与扫码登录。
	AuthDir string
	// ConfigFile 网关配置文件路径，供面板「网关配置」页在线读写。
	ConfigFile string
	// DataDir 面板自有数据的落盘目录（配置备份、登录凭据）。
	// 通常传网关 state_file 所在目录（Docker 里是持久化的 ./data）。
	DataDir string
	// Admin 网关侧账号管理能力（停用/启用/复活）。宿主同进程部署时传入
	// 由 *pool.Pool 适配的实现；独立面板模式留 nil，三个操作会明确报
	// 「当前部署形态不支持」而不是静默失败。
	Admin panelops.AccountAdmin
	// Version 面板版本，仅用于展示。
	Version string
}

// New 装配面板并返回其 http.Handler。
//
// 失败时返回 error 而不 panic：面板是网关的附属能力，宿主应当降级为「仅网关」
// 并把原因打进日志，而不是让整个网关起不来。
func New(opts Options) (http.Handler, error) {
	pcfg, err := panelconfig.Load("")
	if err != nil {
		return nil, fmt.Errorf("加载面板配置: %w", err)
	}

	if opts.GatewayURL != "" {
		pcfg.GatewayURL = opts.GatewayURL
	}
	if opts.GatewayAPIKey != "" {
		pcfg.GatewayAPIKey = opts.GatewayAPIKey
	}
	if opts.AuthDir != "" {
		pcfg.UpstreamAuthDir = opts.AuthDir
	}
	if opts.ConfigFile != "" {
		pcfg.UpstreamConfigFile = opts.ConfigFile
	}
	// 同进程读写同一份 auths/，属主天然一致，无需跨用户 chown。
	pcfg.AuthOwnerUID = -1
	pcfg.AuthOwnerGID = -1

	// 合并后「重启网关」等于重启自己（面板随宿主进程一起重启，中断数秒）。
	// 这对**账号**加载已无必要——账号改由宿主的目录监听热加载（落盘后数秒入池）。
	//
	// 但**配置**变更仍然必须重启：网关只在启动时读 config.json，没有 SIGHUP /
	// 文件监听之类的热重载路径（已确认）。若把 DockerContainer 置空，面板的
	// 「系统」页重启按钮会被禁用，配置页就变成「能存不能生效」的死路。
	// 因此保留该能力（默认 workbuddy2api），由面板自身的 DockerAvailable() 判定：
	// 未挂载 docker.sock 时自动降级为「Docker 不可用」，界面如实提示，
	// 用户改在宿主机执行 docker compose restart 即可。
	pcfg.RefreshConfigOnLogin = false

	dataDir := opts.DataDir
	if dataDir == "" {
		dataDir = "."
	}
	if pcfg.BackupDir == "" {
		pcfg.BackupDir = filepath.Join(dataDir, "backups")
	}
	// 网页改密码需要可写凭据文件；网关 config.json 常为只读挂载，故单独落一份。
	if pcfg.CredentialsFile == "" {
		pcfg.CredentialsFile = filepath.Join(dataDir, "gui-credentials.json")
	}
	// 凭据文件路径确定后再叠加持久化口令（顺序不可颠倒：配置读入时它还是空值）。
	pcfg.LoadStoredCredentials()

	store, err := panelstore.New(pcfg.UpstreamAuthDir)
	if err != nil {
		return nil, fmt.Errorf("初始化面板凭证存储: %w", err)
	}
	gw := panelgateway.New(pcfg.GatewayURL, func() string { return pcfg.GatewayAPIKey }, pcfg.Timeout())
	up := panelupstream.New(pcfg.Timeout())
	svc := panelops.New(pcfg, store, gw, up)
	// 账号管理（停用/启用/复活）走进程内接口，不经网关 HTTP——nil 时保持
	// 「未接入」状态，三个操作会明确报错（独立面板模式）。
	if opts.Admin != nil {
		svc.SetAccountAdmin(opts.Admin)
	}

	static, err := panelwebui.Handler()
	if err != nil {
		return nil, fmt.Errorf("初始化面板前端资源: %w", err)
	}
	h := panelapi.NewServer(pcfg, svc, static, opts.Version).Handler()

	// 启动自检：与网关日志同流（同容器同文件），便于一并排查。
	log.Printf("面板已装配（单端口：/v1/* /status /healthz 归网关，其余归面板）")
	log.Printf("  面板账号:   %s", pcfg.UI.Username)
	log.Printf("  回环网关:   %s", pcfg.GatewayURL)
	log.Printf("  凭证目录:   %s", store.Dir())
	log.Printf("  网关配置:   %s", pcfg.UpstreamConfigFile)
	log.Printf("  数据目录:   %s（备份 %s、凭据 %s）", dataDir, pcfg.BackupDir, pcfg.CredentialsFile)
	if pcfg.UsingDefaultPassword() {
		log.Printf("  ⚠️  面板正在使用默认口令（admin/workbuddy），请立即在 WBGUI_PASSWORD 中修改")
	}
	if pcfg.ReadOnly {
		log.Printf("  面板为只读模式（WBGUI_READ_ONLY=true），一切写操作已关闭")
	}

	return h, nil
}
