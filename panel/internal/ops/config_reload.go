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
type ConfigReloader interface {
	// ReloadConfig 立即按磁盘上的 config.json 重新加载并应用可热生效的部分。
	// 返回错误表示本次没有应用（文件非法 / 读写失败），调用方应据此提示用户；
	// 只改动了需重启字段的情况不算错误（宿主会打日志说明）。
	ReloadConfig() error
}

// SetConfigReloader 注入配置热重载能力（宿主同进程部署时调用）。
func (s *Service) SetConfigReloader(r ConfigReloader) { s.reloader = r }

// NotifyConfigSaved 尽力通知网关「配置已保存」。未注入（独立面板）时返回 (false, nil)。
//
// 失败不阻断保存流程：文件已经写好了，重载失败只意味着「这次没立刻生效」，把保存判为
// 失败反而会误导用户去重试写入。调用方据此给提示文案。
func (s *Service) NotifyConfigSaved() (reloaded bool, err error) {
	if s.reloader == nil {
		return false, nil
	}
	if err := s.reloader.ReloadConfig(); err != nil {
		return false, err
	}
	return true, nil
}
