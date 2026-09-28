# 合并进 workbuddy2api 时对面板源码的改动

本目录（`panel/`）由 `git subtree` 引入自
[`287775856/workbuddy2api-gui`](https://github.com/287775856/workbuddy2api-gui)。

上次同步：面板上游 `a26cd0f`（2026-09-24 提交，2026-09-27 拉取，含 5 个提交）。
当前须保留的改动为第 1–3 条、第 5 条与第 6 条；第 4 条已撤销（原前提失效，见该节）。

本次（`9413e70` → `a26cd0f`）上游带来「模型与倍率」「积分到期」两页、CSRF
反代修复（#7）、外部渠道账号积分补齐（#8），以及 `deploy/nginx.conf.example`。
三处补丁均按原样重放，`App.tsx` 与上游逐字一致。另见文末「已知差异」一节。
**第 6 条例外**：积分到期判据与上游 `a26cd0f` 相反，是按本 fork 的实测改的（见该节）。

为了让它与网关**同进程、同端口**运行，合并时改动了下面几处面板源码。
这些文件上游也会改，因此**每次 `git subtree pull` 后都需要重新确认**。
（纯新增的文件不在此列——它们不冲突，见文末。）

拉取上游后建议先跑：

```bash
go build ./... workbuddy2api-gui/... && go test ./... workbuddy2api-gui/...
```

## 必须保留的改动

### 1. `internal/authstore/preserve.go` + `store.go`（数据丢失修复，**最高优先级**）

**症状**：在面板点一次账号「刷新」，该账号凭证里的 `auth.realm` 与顶层
`device_token` 会被从磁盘上抹掉。

**原因**：面板的 `Store.Save` 是从自己的 `Account` struct 重建整份文档再覆盖文件，
而该 struct 不建模这两个键；网关的 `SaveAtomic`（`internal/auth/auth.go`）会写它们。

**影响**：`realm` 可由网关从 `domain` 反推自愈（`BackfillRealm`）；
`device_token` **不可反推**，丢失即永久失去该账号的 `X-Device-Token` 风控头。

**修法**：新增 `preserve.go`（`readExistingDoc` + `mergeMissingKeys`），在 `Save`
写盘前把磁盘上未建模的键合并进来。采用「保留未知键」而非「补两个硬编码字段」，
以便上游今后新增键时同样不丢。

**回归测试**：`internal/authstore/preserve_test.go`（`TestSavePreservesUnmodeledKeys`）。

### 2. `internal/ops/loginflow.go`（提示语）

`PollLogin` 落盘后的文案原为「请手动重启网关使其加载」等分支。合并后网关有目录
热加载（`cmd/server/reload.go`），数秒内自动入池，故改为
「凭证已保存，网关将在数秒内自动加载（无需重启）」。
重启分支保留，供面板被单独部署 / 网关为旧版本时回退使用。

### 3. `internal/api/server.go`（两处提示语）

- `handleAccountDelete`：`"凭证文件已删除，重启网关后该账号移出账号池"`
  → `"…网关将在数秒内将该账号移出账号池（无需重启）"`。
- `handleConfigPut`：`"…（系统页可一键重启）"` → `"…需重启进程才会生效"`。
  配置确实**不能**热重载（网关只在启动时读 `config.json`，无 SIGHUP / 文件监听），
  故保留「需重启」的准确说法。

### 4. `web/src/App.tsx`（**已撤销**：统计页现已启用）

原先新增 `STATS_ENABLED = false` 常量过滤导航项与路由，理由是「该页依赖网关的
`/v1/stats` 端点，而本上游只注册了 4 条路由」。

**该前提已失效**：上游 PR #161 在面板引入后 34 分钟就补上了 `/v1/stats`
（`internal/server/handler.go`，提交 `733d348`）。2026-09-19 同步面板上游
`9413e70` 时一并撤销此开关，`App.tsx` 已与上游逐字一致。

> 注意区分两种能力：`/v1/stats` 的**累计快照**（按模型聚合）上游已实现，统计页
> 的这部分可用；而面板 `9413e70` 新增的「时间趋势」卡片依赖 `/v1/stats` 的
> **时间维度参数**（`range`/`from`/`to`/`interval`/`model` 与响应的 `range`、
> `series_buckets` 字段），上游网关（`b08f518`）**尚未实现**。故趋势图当前会停在
> 「正在加载趋势数据」（面板作者预留的降级分支），其余功能不受影响。网关侧补齐
> 后该卡片自动开始工作，无需再改面板。

### 5. `internal/webui/dist/index.html`（前端产物）

上游仓库里这份占位 `index.html` 引用的 `assets/index-*.js` 被 gitignore，
**从干净 clone 构建会渲染空白页**（`webui.Handler` 只在 index.html *不可读*时
才回退到「前端未构建」提示页，而这里是可读但资源缺失）。

合并后的 `Dockerfile` 第一个阶段（`node:20-alpine`）会重新构建并覆盖它，
故这份文件的内容不重要；**但不要删除它**——`//go:embed all:dist` 需要目录非空。

`9413e70` 同步时上游更新了此文件引用的 hash（`index-BQ3iX5L-` → `index-BEIjNl4j`）。
已在 `panel/web/` 跑 `npm run build` 重新生成，产物 hash 与引用一致；源码模式下
（不经 Dockerfile）也能正常渲染。

### 6. 积分到期判据改回 `CycleEndTime`（**与上游相反，务必重放**）

上游 `a26cd0f` 把到期判据从 `CycleEndTime` 改成 `DeductionEndTime`（毫秒时间戳），
理由是「按 `DeductionEndTime` 排序与官方平台奖励积分明细逐行一致」。**该结论是错的**，
本 fork 已改回 `CycleEndTime` 优先、`DeductionEndTime` 仅作回退：

- `internal/upstream/client.go`：`expiryString()` 与 `resourcePackage`/`CreditPack` 的字段注释
- `web/src/types.ts`：`CreditPack.end_time` / `cycle_end_time` 的文档注释
- `web/src/pages/Credits.tsx`：表头说明与「周期至 …」小字的 tooltip

**为什么上游错了**：「个人体验版」的 `DeductionEndTime` 是 `2034-12-22` / `2034-10-13`，
与注册日同月日、恰隔 10 年——这是**账户级的登记上限**，不是这批积分的作废时刻。
实证：该包 `CycleEndTime` 是月末 23:59:59（本周期边界），且**上游扣费时先扣这个包**
（余额在动），而到期更晚的包分文未动。上游据「明细页排序一致」推断，但那只反映
**相邻两次扣费的展示顺序**，不是「何时作废」——把展示顺序当成了扣费顺序。
按上游口径，该包会显示成 2034 年到期，`CycleEndTime` 一到积分就作废而页面毫无预警。

**连带**：根网关 `internal/upstream/expiry.go` 的 `packageExpiryTime` 是同口径，
选号 72 小时紧急优先依赖它——不重放此改动，面板会显示「2034 年到期」而网关其实
已按月末判定，两边自相矛盾。

`panel/web/` 改动后须重跑 `npm run build`（产物 hash 会变，`index.html` 需同步提交）。

## 纯新增、不会冲突的文件

| 文件 | 作用 |
|---|---|
| `host/host.go` | 面板装配包。**刻意不含 `internal` 段**，宿主导入合法（Go 的 internal 规则不允许根 module 直接 import `workbuddy2api-gui/internal/*`）。 |
| `internal/authstore/preserve.go` | 见上。 |
| `internal/authstore/preserve_test.go` | 见上。 |
| `HOST-PATCHES.md` | 本文件。 |

宿主的对应文件在仓库根：`cmd/server/panel.go`（路由合并）、`cmd/server/reload.go`
（账号热加载）。它们不属于 subtree，不受 pull 影响。

## 已知差异（未改上游代码，仅记录）

### 模型页的「不要加域前缀」与本网关的 `cn:` 前缀

上游 `a26cd0f` 新增的「模型与倍率」页（`web/src/pages/Models.tsx`）说明文字写：

> 调用时**直接填第一列的模型 ID**（如 `glm-5.3`）—— 网关按账号所属域自动路由，
> 不要加「域前缀」，上游不认这种写法。

该结论来自面板作者对**上游官方网关**的实测（带前缀调用被拒：
`{"code":11102,"msg":"model [cn:glm-5.1] service info not found"}`）。但**本 fork
的网关是加前缀的**，两处口径不同：

- `internal/server/handler.go` 的 `modelList` 对 CN 模型输出 `"id": "cn:" + mi.ID`
  （global 为 `"global:" + id`），故第一列小字副行展示的是**带前缀**的 id；
  相关断言见 `internal/server/handler_context_length_test.go`（如 `byID["cn:glm-5.2"]`）。
- `internal/server/resolve_model.go` 的 `resolveModel`：裸名一律判为 `realm=cn`；
  而 `realm` 参与选号（`handler.go` 中 `acct.Realm() != realm` 的过滤），
  **不剥离前缀、也不是「按账号所属域自动路由」**。

因此在本 fork 上：

- 填**裸名**（`glm-5.3`）等价于 `cn:glm-5.3`，走国内版号；国内版模型这样写没问题。
- 填**带前缀名**（照抄副行 `global:xxx`）是**可用**的，与本页文案相反。
- 只有国际版账号时，填裸名会按 CN 域筛号而选不到号——需填 `global:` 前缀。

未改动面板源码：该文案对上游是准确的，属两套网关的语义差异，不宜把面板改成
与本 fork 强绑定。**若今后要让本 fork 的 `/v1/models` 返回裸 ID**（与上游对齐），
那是一次网关侧的行为变更，需同步改上述测试与 `resolveModel` 的默认域语义。
