// config_reload.go 面板保存配置后**立即**通知网关热重载。
//
// 为什么需要：网关热重载本身是**按需触发**的（cmd/server/configreload.go），不做任何轮询
// ——它假定写入方会主动通知。面板正是那个写入方：点下按钮就知道文件已经写完，此时直接
// 调宿主注入的回调，把「保存」与「生效」压成同一步。
//
// 接口定义在消费方（本包）而非实现方：Go internal 规则不允许 workbuddy2api-gui module
// import workbuddy2api/internal/*，故由宿主在 cmd/server 里以适配器实现（同 AccountAdmin）。
//
// 独立面板模式（panel/cmd/server 连远端网关）下未注入：此时代码路径上没有同进程的网关可
// 通知，改动需重启网关进程才生效。未注入不报错，保存本身仍然成功。
package ops

// ConfigReloader 网关侧的配置热重载能力（宿主进程内注入）。
//
// 返回两个字段清单而不是一个 bool：面板要用它区分「这次保存全部生效」与「其中某几项
// 只能重启后才生效」。只返回 error 的话，宿主「只改了冷字段、什么都没应用」与「全部
// 热生效」在面板眼里完全一样，于是提示语必然说谎。
type ConfigReloader interface {
	// ReloadConfig 立即按磁盘上的 config.json 重新加载并应用可热生效的部分。
	//
	// hot 是本次已立即生效的顶层字段，cold 是本次改动里只能重启进程才生效的顶层字段。
	// 返回错误表示本次没有应用（文件非法 / 读写失败），调用方应据此提示用户；
	// 只改动了需重启字段的情况不算错误（此时 hot 为空、cold 非空）。
	ReloadConfig() (hot, cold []string, err error)
}

// ReloadReport 一次保存后的生效结论。
//
// 刻意用面板自己的类型承载：宿主 module 不能 import 本包（Go internal 规则），
// 跨边界只传 []string / error 这类内置类型，两侧各自组装。
type ReloadReport struct {
	// Reloaded 是否真的通知到了网关并成功执行重载。false 有两种原因：独立面板
	// 模式未注入回调，或重载报错（此时 Err 非空）。
	Reloaded bool
	// Hot 本次已立即生效的顶层字段（可能为空：只改冷字段、或内容没变化）。
	Hot []string
	// Cold 本次改动里只能重启进程才生效的顶层字段。
	Cold []string
}

// SetConfigReloader 注入配置热重载能力（宿主同进程部署时调用）。
func (s *Service) SetConfigReloader(r ConfigReloader) { s.reloader = r }

// NotifyConfigSaved 尽力通知网关「配置已保存」。
//
// 未注入（独立面板）时返回零值 ReloadReport 与 nil error —— Reloaded=false 让调用方
// 判定「改动需重启网关进程才生效」。失败不阻断保存流程：文件已经写好了，重载失败只
// 意味着「这次没立刻生效」，把保存判为失败反而会误导用户去重试写入。
func (s *Service) NotifyConfigSaved() (ReloadReport, error) {
	if s.reloader == nil {
		return ReloadReport{}, nil
	}
	hot, cold, err := s.reloader.ReloadConfig()
	if err != nil {
		return ReloadReport{Cold: cold}, err
	}
	return ReloadReport{Reloaded: true, Hot: hot, Cold: cold}, nil
}
