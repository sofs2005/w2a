# 合并进 workbuddy2api 时对面板源码的改动

本目录（`panel/`）由 `git subtree` 引入自
[`287775856/workbuddy2api-gui`](https://github.com/287775856/workbuddy2api-gui)。

上次同步：面板上游 `a26cd0f`（2026-09-24 提交，2026-09-27 拉取，含 5 个提交）。
当前须保留的改动为第 1–3 条、第 5–15 条；第 4 条已撤销（原前提失效，见该节）。
（第 9–11 条与第 12 条均为本 fork 独有的新增能力/删除，第 13 条为跟随网关的键改名，
第 14 条为别名可观测性（新增统计列），第 15 条为出站代理，同属「必须保留」。）

本次（`9413e70` → `a26cd0f`）上游带来「模型与倍率」「积分到期」两页、CSRF
反代修复（#7）、外部渠道账号积分补齐（#8），以及 `deploy/nginx.conf.example`。
三处补丁均按原样重放，`App.tsx` 与上游逐字一致。另见文末「已知差异」一节。
**第 6 条例外**：积分到期判据与上游 `a26cd0f` 相反，是按本 fork 的实测改的（见该节）。
**第 8 条之后**：因新增「模型别名」页，`App.tsx` 重新出现差异（导航项 + 路由），
上游若再改此文件需一并重放。

为了让它与网关**同进程、同端口**运行，合并时改动了下面几处面板源码。
这些文件上游也会改，因此**每次 `git subtree pull` 后都需要重新确认**。
（纯新增的文件不在此列——它们不冲突，见文末。）

> **维护方式变更（2026-10-15 起）**：面板源码**直接维护**，不再走「补丁台账 → pull
> 后逐条重放」的流程。本文件的定位随之从「重放脚本」降为**差异清单**：它的作用只剩
> 「下次 `git subtree pull` 时对照着看哪些文件被本 fork 动过、冲突该怎么合」，而不是
> 一份需要机械执行的补丁序列。改动直接写进源码、连同网关侧一起提交，理由有二：
>
> 1. 重放流程的实际成本与收益已经倒挂——面板文件被本 fork 改得越来越多（下面第
>    1–15 条），每次 pull 都要逐条重放再逐条验证，而漏放的症状（如第 8 条的
>    `realms` 静默丢弃）恰恰是单元测试全绿、面板显示错误，最难发现；
> 2. 本 fork 与上游的分叉已不可逆（双 realm、别名、积分保底、按域统计都是本 fork
>    独有），「与上游逐字一致」不再是目标，「差异可控且可追溯」才是。
>
> 因此下面各条仍然逐条保留（它们是差异的说明），但 pull 后请把它们当作**检查项**
> 而非**执行项**：对着看、按需合并，而不是无条件重放。

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
> `series_buckets` 字段），上游网关（`b08f518`）**尚未实现**。

**后续修订（本 fork 实测反馈）**：原先的做法是「保留降级分支、等网关补齐后自动生效」，
但实测下这个降级分支是个**永不消失的「正在加载趋势数据…」空占位**，比不显示更误导；
且它旁边两句文案与实现相反 —— `series_buckets` 网关从未赋值（「可回溯 N 个时间桶」永不显示），
而「按小时落盘、保留 30 天、重启不清零」纯属虚构（统计只在内存，进程重启即清零）。
故本 fork 改为**暂时隐藏**：

- `web/src/pages/StatsPage.tsx`：把「时间趋势」卡片**整块**（含卡片本身）与配套的
  6 个时间维度 state 注释掉 —— 页面上不再出现该模块，也不留空占位；同时修正
  「统计范围」里「持久化在 data 目录，重启不丢」的错误说法。
- 一并移除「模型筛选」下拉：它只是把 `model` 参数发给网关，而网关不解析该参数，
  表格用的也是未过滤的 `resp.stats.models` —— 属于**同一根因下的另一个死开关**。
- `load()` 不再发送时间维度参数（网关忽略），`api.stats` 的签名保持不动。

**恢复方式**：网关侧补齐时间序列后，取消 `StatsPage.tsx` 中两处注释块（顶部 `TrendChart`
import 与 state 声明、卡片本体），并把 `load()` 的时间参数加回 —— 恢复步骤已写在注释处。
`types.ts` / `api.ts` / `TrendChart.tsx` **刻意未改**，故这三处无需恢复（`TrendChart.tsx`
现在是未被引用的孤儿文件，保留即为了这层零成本恢复）。

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

### 7. `web/src/pages/ConfigPage.tsx`（定时任务表单）

原表单只有「签到 / 保活」两项，与网关实际的**七类**排程（签到 / 旅行 / 活跃上报 /
保活 / 开学季 / 夜猫子 / 成长任务补跑）长期不一致，且两处文案是错的：

- 「签到同时会推进猫猫旅行」——旅行早已剥离为独立排程（`travel_hours`）。
- 「关闭后签到与猫猫旅行都会停摆（旅行搭签到便车）」——同上，`checkin_enabled=false`
  不再影响旅行。

本次补齐七类的小时 + 开关字段，并修正上述文案。**关键点**：新增的
`school_/cat_/growth_enabled` 在老 `config.json` 里**不存在**，而网关侧语义是
「缺省 true，只有显式 false 才关」（`internal/config/schedule.go` 的
`DefaultSchedule` + `Normalize`）。原先表单的 `bool()` 是 `=== true`，会把这些
键渲染成**未勾选**——用户随手保存一次就把三类任务全关掉。故本页新增 `defaultOn()`
（`undefined/null` → true），七处 schedule 开关改用它。`bool()` 保留原语义，
供「会话粘性」等缺省即关的开关使用。

**上游影响面**：上游也在改这个文件（「定时任务」卡片所在），`git subtree pull`
后需重放本节的字段与 `defaultOn()`。

`panel/web/` 改动后须重跑 `npm run build`（产物 hash 会变，`index.html` 需同步提交）。

### 8. `web/src/pages/ModelAliases.tsx` + 别名端点（**新增页面**）

配合根网关的「统一 cn/global 调度 + 别名映射」改造（`internal/aliases/`、
`cmd/server/reload.go` 的 `startAliasWatcher`），面板新增「模型别名」页维护
`model_aliases.json`。

**为什么独立成文件、独立成页**：别名是**高频调整**的运维参数（"这个模型两域名不一样"、
"隐藏模型只在一域有"），而网关只在启动时读 `config.json`（无 SIGHUP / 文件监听）。
放 config 里就得每次重启容器；独立文件 + 5s 轮询让它数秒内生效。因此本页的提示语
与「网关配置」页**刻意不同**：那句「修改后需要重启网关才生效」在这里是错的。

**改动点**：

- `internal/config/config.go`：新增 `alias_file`（`WBGUI_ALIAS_FILE`）。
  **留空是正确取值**——`ops.Service.AliasFilePath` 从网关 `config.json` 的
  `state_file` 推导同目录的 `model_aliases.json`，与网关 `cmd/server/main.go` 的
  `aliasFilePath` 同一规则。两边必须算出同一个路径，否则表现为"保存成功但没生效"。
  显式配置只在部署布局特殊时才需要。
- `internal/ops/aliasfile.go`（纯新增）：`ReadAliases`/`WriteAliases`/`ResetAliases`，
  复刻 `configfile.go` 的 `.gui.bak` 备份 + `fsutil.WriteFileAtomic` 原子替换。
  **额外做面板侧校验**（与 `internal/aliases.Parse` 同口径）：网关对非法表是
  「静默保持旧表 + WARN」，用户从界面看不出自己刚保存的东西没生效，故在这里
  前置拦下并给出带行号的原因。
- `internal/api/server.go`：`GET/PUT /api/model-aliases` + `POST …/reset`
  （reset 走 `ensureDangerous`，与 config reset 一致）。
- `web/src/pages/ModelAliases.tsx`、`App.tsx`（导航项 + 路由）、`api.ts`、`types.ts`。
- `web/src/realm.ts` + `web/src/pages/Models.tsx`（**上游文件，需重放**）：模型按域分栏
  原先靠 id 前缀（`global:` = 国际版）。统一调度后 `/v1/models` 只输出裸名，前缀没了，
  分栏会全部落到「国内版」。改为 `modelRealms(m)`：优先读服务端下发的 `realms` 字段，
  缺席（老网关）时回退按前缀推断——两个版本的网关都能正确分栏。`Models.tsx` 的三处
  `modelRealm(m.id)` 调用点、`realms.length === 2` 的「双域」副行标注，以及页脚文案
  同步更新（说明前缀现为网关侧「钉域」扩展）。
- `internal/gateway/client.go`（**上游文件，需重放**）：`Model` 结构体补 `Realms` 字段。
  **这是上面那条前端改动的必要前提**——`gateway.Model` 是白名单式结构体，
  `encoding/json` 对未声明字段**静默丢弃**，网关下发的 `realms` 到不了前端；
  `modelRealms` 于是永远走前缀回退分支，域分栏全部落进「国内版」，且没有任何报错
  可循（前端那个"回退"反而把问题盖住了）。回归测试 `internal/api/models_test.go`
  的 `TestModelRealmsPassthrough` 钉住该字段的解码与再编码两段链路。

**后续修订（同日，用户实测反馈）**：

1. **三列下拉候选按域切分**。原实现三列共用一份 `datalist`（两域并集），后果是
   「国际真实名」格里也会列出只在 CN 存在的模型，选它等于配了一条永远走不通的映射。
   改为 `alias-catalog-all` / `-cn` / `-global` 三份，表头标注各列候选数量
   （`共 N 个`）。域判定复用 `modelRealms`，与「模型与倍率」页同一口径。
2. **`color-scheme: dark`**。`styles.css` 从未声明它，于是原生 `<datalist>` 弹层
   按浅色渲染（白底黑字），与整站深色主题格格不入。声明在 `:root`（可继承，覆盖
   全部原生控件）。
3. **新增「两域同名，无需配置」只读卡片**。同名模型写进别名表在路由上是 **no-op**
   （裸名本就跨域通用），自动写入只会把用户的手工条目淹没在几十行噪声里。故只做
   只读展示 + 一个「从目录导入这 N 条」按钮（点了才填表格，保存才落盘）。
   `realms` 缺席（老网关）时不显示该卡片，改提示"无法按域区分"。
4. **下拉框改用自绘 `SuggestInput`（`web/src/SuggestInput.tsx`，纯新增）**。
   按域切分候选后用户仍报「国际列点上去没有下拉」，且**弹出的是自动填充密码**——
   这是浏览器密码管理器抢占：面板登录页存了口令，Chrome 把紧随其后的文本框当成新的
   凭据字段，`<datalist>` 的原生弹层根本没机会显示。这类拦截**无法从应用侧关闭**
   （`autocomplete="off"` 对密码管理器只是建议），只能不用原生弹层。
   故改为自绘：`position: fixed` + 实测坐标（不受祖先 overflow 裁剪）、输入即过滤、
   键盘上下选择/回车确认/Esc 收起。刻意**不放进 `ui.tsx`**——那是上游文件，放进去
   会给 subtree pull 增加一处冲突面。

**回归测试**：`internal/api/alias_test.go`（含「推导路径必须与网关一致」
「非法输入不得改动磁盘文件」两条要害断言）。

`panel/web/` 改动后须重跑 `npm run build`（产物 hash 会变，`index.html` 需同步提交）。

## 9. 账号台账透传：`internal/gateway/client.go` + `internal/ops/ops.go` + `web/src/pages/Accounts.tsx`

**动机**：网关 `/status` 每个账号都带 `realm`、`rate_limited_models`（6004 模型级
限流台账）、`model_costs`（每模型实测成本台账），但面板侧 `gateway.AccountStatus`
**一个都没声明** → `encoding/json` 静默丢弃 → 面板上完全看不到。这与第 8 条的
`realms` 是同一个坑，而后果更重：`/v1/stats` 只按**请求体的模型字符串**聚合、不区分
账号，统一调度后裸名又会在 cn/global 账号间调度，于是「同一个裸名，国内号花了多少、
国际号花了多少」在面板上**没有任何出口**——只有这两份台账能答。

**改动**（三处，全部是新增字段，无逻辑改写）：

1. `internal/gateway/client.go`：`AccountStatus` 增 `Realm` / `RateLimitedModels` /
   `ModelCosts`，并新增两个从属结构体（与 `pool.RateLimitedModel`、
   `pool.ModelCostStatus` 逐字段对齐）。**声明纪律**写进了结构体注释：网关 `/status`
   下发的每个运维字段都必须在此显式声明，否则静默丢弃。
2. `internal/ops/ops.go`：`AccountView` 增同名字段，`Accounts()` 的 merge 循环里逐字段
   搬运（`v.Realm = ga.Realm` 等三行）。**漏搬一行与漏声明一样静默**，故
   `account_ledger_test.go` 真起假网关走完 `Accounts()` 全路径来钉住这几行。
3. `web/src/pages/Accounts.tsx`：账号行显示域标签（`realmLabel(acctRealm(a))`）与限流
   模型数；详情弹窗新增 `AccountLedgers` 组件渲染两张台账表。

**上游冲突面**：`client.go` / `ops.go` / `Accounts.tsx` 都是上游文件，三处改动均为
「新增字段 + 新增渲染块」，未改既有字段语义与既有渲染逻辑，重放时应能机械合入。
`AccountLedgers` 是 `Accounts.tsx` 内新增的顶层函数（未拆新文件，避免为一个小块再添
一个模块；若上游同时改了该文件尾部需手工拼接）。

**已知差异**：`acctRealm()` **优先取网关下发的 `realm`**，缺席才回退按 `domain` 推。
选号是按 `realm` 过滤的，展示口径必须与之一致——两者不一致时（凭证被手工改过等）
不能给出与选号矛盾的结论。

## 10. 倍率与促销按域拆分：`internal/gateway/client.go` + `web/src/pages/Models.tsx`

**动机**：统一调度改造（本仓库 `d2c52be`）把 `/v1/models` 输出从 `cn:x` / `global:x`
双条目改成**裸名并集去重**，同名模型合并成一条。合并规则是 CN 优先（见网关
`handler.go` 的 `mergeMissingFields`），于是两域倍率不同时 global 的值被丢掉，
「模型与倍率」页的国内版/国际版两个 tab 渲染同一个数——**国际版显示的是国内价**。
倍率确实分域：网关侧 `upstream.modelRates` 本就按 `realm` 分桶存储，选号（积分保底、
成本分层）也按账号自身域各查各的，只有展示侧塌了。

**改动**：

1. `internal/gateway/client.go`（**上游文件，需重放**）：`Model` 增 `CreditsCN` /
   `CreditsGlobal` 两个字段（`json:"credits_cn,omitempty"` / `credits_global`）。
   同第 8、9 条的坑——白名单结构体，漏声明即静默丢弃。
2. `internal/api/server.go`：`modelView` 增 `PromotionsCN` / `PromotionsGlobal`。**并顺带
   修掉一个真 bug**：旧的 `modelRealmBare` 只认 `"cn:"` / `"global:"` 前缀，而统一调度
   后目录只剩裸名，于是所有模型都落进 CN 分支 → **国际版 tab 恒显示国内活动**。改为按
   `modelRealms(m.ID, m.Realms)`（网关 `realms` 字段优先，前缀只作老网关回退）取域集合，
   逐域配对促销。配对逻辑从 `handleModels` 的 HTTP 壳里抽成纯函数
   `buildModelViews` / `matchRealmPromos` 才测得动（旧实现没测试够得着，bug 才活了下来）。
3. `web/src/types.ts` / `web/src/pages/Models.tsx`：`Model` 增两个分域倍率字段；新增
   `realmCredits(m, realm)` / `realmPromos(m, realm)` 两个按当前 tab 取的助手，倍率列、
   促销列、`onlyFree` / `onlyPromo` 过滤、`freeCount`、排序用的 `modelPromo(m, realm)`
   全部改为按域取；页脚注明该 tab 显示的是本域价。合并字段 `credits` 保留为兼容回退
   （老网关或该域上游未标倍率时），不是主展示源。

**回归测试**：`internal/api/models_test.go` 新增三条——`TestModelCreditsPerRealmPassthrough`
（分域倍率的解码 + 再编码两段链路，含缺席侧不得序列化出来）、
`TestBuildModelViewsPromotionsPerRealm`（同一裸名两域活动完全不同时各取各的、单域条目
不串味、老网关前缀回退）、`TestModelBareAndRealms`（前缀剥离 + 域顺序契约）。
网关侧同步改了 `internal/server/handler_global_models_test.go` 的旧 CN-first 断言
（它钉的正是"只留合并值"这个旧口径）。

**上游冲突面**：`client.go` 是新增字段；`Models.tsx` 改了倍率列/促销列的渲染取值处
（同第 8 条那几处 `modelRealm` 调用点），`types.ts` 增字段——重放时若上游同时改了
`Models.tsx` 的渲染块需手工拼接。`internal/api/server.go` 的 `modelView` 与
`buildModelViews` 改动面较大，上游若重写该 handler 需整体重放本条的语义。

## 11. 请求统计按域分行：网关 `/v1/stats` + `web/src/pages/StatsPage.tsx`

**动机**：第 10 条把「模型与倍率」页的倍率/促销按域拆开了，但**请求统计**页还塌着：
网关 `metricsStore` 用**请求体里的模型名原文**做键（`global:x` 与裸名 `x` 各占一行），
统一调度后同一个模型在 CN 与 global 账号之间调度，两域的单价、限免、上下文都不同，
却混进同一个累加器——只能看到一个加权平均，既看不出哪域花了多少，也解释不了扣费
为什么变。用户要求：其余列拆成国内/国际两行（当时还要求「官方价放裸名后面」，
该列已在第 12 条整块移除）。

**改动**：

1. 网关 `internal/server/metrics.go`：`metricsStore` 从扁平 `byModel` 改为**一棵两层树**
   `byBare map[string]map[string]*modelMetrics`（裸名 → 域 → 累加器）。
   - 键用 `resolveModel()` 剥出的**裸名**：`cn:x` 与裸名 `x` 指的是同一个底层模型，
     只有裸名能让它们合成表格的一行。
   - 第二层是**承接请求的账号域**（`chatStat.realm`，见 `internal/server/logging.go`），
     **不是**请求体前缀——前缀只表达"允许打哪"，实际落在哪个域由选号决定；账要记在
     实际提供服务的那一侧。空域 `""`（选号失败 503、模型名解析不出）独立成组，
     不并进任何一侧：编造归属比空着更糟。
   - 容量上限仍按**裸名个数**计（子条目数 = 裸名数 × 域数 ≤ 3，有界）。
   - `ModelStatPayload` 增 `Bare` / `Realms []RealmStat`；父行的派生量（均值/比率/吞吐）
     由**原始量合计反算**而不是把各域均值再平均（后者会让请求数少的域被等权放大）。
   - `enrichCredits` 的注释同步（键已是裸名，仍走 `resolveModel` 只为兜住异常串）。
2. 网关 `internal/server/handler.go`：选号成功后 `st.realm = acct.Realm()`（与
   `st.uid` / `st.nick` 同一处）。选号失败路径不赋值 → 空域，正是想要的语义。
3. `internal/gateway/client.go`（**上游文件，需重放**）：`ModelStat` 增 `Bare` /
   `Realms`，并新增 `RealmStat` 结构体。同第 8～10 条的坑——白名单结构体，
   漏声明即**静默丢弃**整段（`realms`、`credits` 在本项目已各出过一次）。
4. ~~`internal/api/server.go`：官方价换算的键从 `m.Model` 改为 `statPriceKey(m)`~~
   **已随第 12 条整块移除**（`statPriceKey` 与官方价换算一同删除）。
5. `web/src/types.ts` / `web/src/pages/StatsPage.tsx`：`ModelStat` 增 `bare` / `realms`，
   新增 `RealmStat`。表格改成「每行 = 一个裸名 + 其下按域分行的明细」：抽出新组件
   `StatRow`，**父行不再渲染任何数值列**（父行数值无法归属到任何域，画出来正是这次
   要消除的混淆），只承载裸名（第 12 条后又去掉了官方价单元格）；域徽章复用 `realm.ts` 的 `realmLabel`
   （国内版/国际版），空域显示「未路由」。排序仍按父条目（裸名合计）——按单域排序会
   让模型位置随"哪个域更忙"跳变。
   降级：`realms` **整段缺席**（老网关/手写载荷）时退化为一行「未标注域」，不按前缀猜域。
   **注意单域模型也会带一条 `realms`**（数组长度 1）：它确实知道自己在哪一域跑的，
   标成"未知域"是信息倒退——所以单域模型显示「国内版」/「国际版」徽章，不是「未标注域」。
   父行与唯一子条目等值（有测试钉住）。

**回归测试**：网关 `internal/server/metrics_test.go` 新增四条——`TestMetricsSplitsByAccountRealm`
（同一裸名两域各若干请求 → 一行两域明细、父行由合计反算而非均值再平均、域序 cn→global）、
`TestMetricsBareKeyCollapsesRealmPrefix`（`cn:x` 与裸名 `x` 合成一行）、
`TestMetricsUnroutedRealmIsOwnGroup`（空域独立成组且不并进 cn）、
`TestMetricsRealmOrderUnknownRealmLast`（异常域值按字典序补尾，输出确定）。
`metrics_credits_test.go` 的 `TestStatsCreditsGlobalRealm` 改了旧断言（它钉的正是
"前缀名与裸名各占一行"的旧口径）。面板侧 `internal/api/models_test.go` 的分域用例不受影响。

**上游冲突面**：`client.go` 是新增字段/新结构体；`StatsPage.tsx` 的按模型明细表格整块
重写（`<tbody>` 从内联 map 改为 `<StatRow>` 组件），上游若重写该表格需整体重放本条的
语义；`types.ts` 增字段。网关侧 `metrics.go` 是**本 fork 独有**的聚合实现（上游 `/v1/stats`
无时间维度），`handler.go` 只加一行赋值。

## 12. 移除「官方 API 价格换算」（统计页 + 面板后端）

**动机**：该功能是上游面板自带的（`internal/pricing` 包 + 统计页的官方价列 / 「编辑价格」
弹窗），把 token 用量按厂商官网单价折算成「走官方 API 要花多少钱」。但它在本部署里
既不可用也无用：

- 内置价格只有 DeepSeek（`pricing.Default()`），其余厂商定价页是 JS 渲染抓不到，
  上游故意留空——所以表格里绝大多数模型恒显示「未配置」。
- 「编辑价格」按钮的可用性绑在面板配置 `pricing_file` 上（`editable = cfg.PricingFile != ""`），
  而本 fork 是**单进程部署**：面板配置只认 `WBGUI_*` 环境变量（见 `host/host.go` 的
  `panelconfig.Load("")`），`pricing_file` 从未被设置 → `editable` 恒 false → 按钮恒灰。
  页面上「点『编辑价格』填写后即可看到」的提示因此永远无法兑现。
- 上游的 `host.go` 对 `BackupDir` / `CredentialsFile` 都做了「为空则推导到 DataDir」的兜底，
  唯独漏了 `PricingFile`——这正是按钮恒灰的根因。

用户判定该换算本身无用，要求整块去掉。

**改动**（相对上游是**删除**，不是改写）：

1. 删除整个 `internal/pricing/` 包（含 `pricing_test.go`）。
2. `internal/api/server.go`：删 `PUT /api/pricing` 与 `DELETE /api/pricing/{model}` 两条
   路由及 `handlePricingUpdate` / `handlePricingDelete`；`handleStats` 只回 `{"stats": st}`
   （不再算 `costs` / `total` / `priced` / `unpriced` / `pricing`）；删 `statPriceKey` 与
   `pricing` import。
3. `internal/ops/ops.go`：删 `Service.pricing` 字段、`New` 里的 `pricing.New(cfg.PricingFile)`、
   `Service.Pricing()` 方法与 import。
4. `internal/config/config.go`：删 `PricingFile` 字段与 `WBGUI_PRICING_FILE` 环境变量；
   `config.example.json` 删 `pricing_file` 键。
5. `web/src/api.ts`：删 `savePrice` / `deletePrice`，以及 `stats()` 的 `mode` 参数。
6. `web/src/types.ts`：删 `ModelPrice` / `ModelCost` / `PricingTable`；`StatsResponse`
   只剩 `stats`。
7. `web/src/pages/StatsPage.tsx`：删 `PriceCell` / `PriceEditor` 组件、`editingModel` /
   `timeMode` state、「官方 API 价格换算」整张卡片、汇总卡片的「官方 API 应付」项、
   表格的「官方价」列、以及「指标说明」里的「官方价」/「官方应付」两条。**保留**「模型」列
   （裸名 + 合计请求数 + 最近请求时间）——它是分域行的组头，与价格无关。

**不受影响**：账号页的 `model_costs`（`ModelCostStatus`）是**网关侧实测账本**
（按 `usage.credit` 折算的每千 token 实际单价，见第 9 条），与官方价无关，保留不动。

**回归测试**：`pricing_test.go` 随包删除；两个 module 的 `go test ./...` 与前端
`tsc --noEmit` 全绿。

**上游冲突面**：本条与上游对 `StatsPage.tsx` / `api.ts` / `types.ts` / `server.go` /
`ops.go` / `config.go` 的任何改动冲突（尤其上游若增强官方价功能）。将来若想把该功能
收回来，除恢复 pricing 包与上述后端接线外，**必须**在 `host/host.go` 里补 `PricingFile`
的默认路径（`filepath.Join(dataDir, "pricing.json")`），否则按钮照旧恒灰。

### 13. `web/src/pages/ConfigPage.tsx`（积分保底改为按域双值）

**动机**：网关侧积分保底已从单值 `pool.credit_floor` 改为**按域分档**
（`pool.credit_floor_cn` 默认 100 / `pool.credit_floor_global` 默认 30，见根仓库
`cmd/server/config.go`）。旧单值键已废弃：网关只把它当**兼容迁移入口**（非 0 时铺到
两域，且不覆盖显式新键），面板若继续写它，用户从界面「改保底」会**静默失效**
（网关的迁移只在两域仍为默认值时生效，等于改不动）——这正是本次必须同步的原因。

**改动**：把该卡片唯一的单值输入框换成两个（国内 / 国外），placeholder 分别为
`100` / `30`。**留空 = 不写该键**（`numOrUndefined('')` → `undefined` → 保存时从文档
移除该键 → 网关取默认）；**显式填 0 = 关闭该域**——这一区分依赖网关 `Default()` →
`Unmarshal` 的顺序（键缺席保留默认、显式 0 落 0），已在网关侧测试钉住
（`cmd/server/credit_floor_config_test.go` 的 `TestCreditFloorExplicitZeroDisables`
与 `TestCreditFloorDefaultFromEmptyFile`）。

**说明文字的放置**：该说明为两格共用，故**放在 `.row` 之外、自成一行**（包一层
`.field` 以复用 `.field .desc` 样式）。不要把它塞回任一格内：`.row` 是
`align-items: flex-end`（按底部对齐），多出说明的那一格会变高、把两格的 label 与
输入框顶成垂直错位。重放本补丁时留意这一点。

**上游冲突面**：与第 7 条同在 `ConfigPage.tsx`，且就在「闲置补偿」行下方；上游若
改动该卡片需一并重放。`panel/web/` 改动后须重跑 `npm run build`（产物 hash 会变，
`index.html` 需同步提交）。

## 14. 统计页「出站模型」列：网关 `/v1/stats` 增 `out_models` + `StatsPage.tsx`

**动机**：模型别名（第 8 条）此前**不可观测**。想回答「我配的别名到底生效没有」，
三条现成通道全都答不了：

- 统计表的行键、网关流水行都只回显**入站**名（客户端写的那串），别名改写在出站侧，
  两处都看不到；
- 响应体里的 `model` 是**上游回显**——上游会把自己的别名规范化掉（请求 `xxx-f`
  也回 `xxx`），于是「网关改写了」与「网关没改写」在上游看到的完全相同；
- 结果是：别名生效 / 不生效在面板上长得一模一样，只能靠 CPA 之类的外部工具反推，
  而那次反推的结论恰好又是上游规范化造成的假象。

**改动**：

1. 网关 `internal/server/logging.go`：`chatStat` 增 `outModel`（出站物理名）。
   赋值点与 `realm` 同处（`handler.go` 的 `rewriteModel` 旁，选号成功之后）——
   选号失败 503 时保持空串，与 `realm` 同一条件。
2. 网关 `internal/server/metrics.go`：`modelMetrics` 增 `outModels map[string]bool`
   （**出站名集合**）；`accumulateMetric` 收集、`addMetric` 并集；`RealmStat` 增
   `OutModels []string \`json:"out_models,omitempty"\``，由新增的 `outModelsOf` 填充。
   - **落在域子条目而不是父条目**：出站名由选中账号的域决定，同一对外名在 cn 与
     global 可映射到两个不同物理名；放在父条目上只剩一个并集，正好丢掉要查的东西。
     这也是 `RealmStat` 唯一一个跨域字段（其余标识字段都归父条目，见该结构体注释）。
   - **只在「出站名 ≠ 裸名」时输出**（`outModelsOf` 把空串与等于裸名的都滤掉，
     滤空返回 nil）：非别名请求出站名 == 裸名，输出纯属噪音。**故普通请求的载荷与
     本字段引入前逐字节一致**——面板靠「字段缺席」把「确认未改写（同名）」与
     「网关没上报」区分开，恒输出空数组会让每条请求都被读成"改写过了"。
3. `internal/gateway/client.go`（**上游文件，需重放**）：`RealmStat` 增 `OutModels`。
   同第 8～10 条的坑——白名单结构体，漏声明即**静默丢弃**（`realms`、`credits`
   在本项目已各出过一次；本次漏了的话症状正是那一列恒显示「同名」，即"没走别名"，
   恰好是这一列要回答的问题本身）。
4. `web/src/types.ts` / `web/src/pages/StatsPage.tsx`：`RealmStat` 增 `out_models`；
   「按模型明细」表在「域」右侧插入**出站模型**列（新增 `OutModelCell` 组件）。
   显示口径：
   - 有值 → 高亮显示物理名（多值逗号并列，正常 1 个）；
   - 缺席 → 低调显示**「同名」**（= 出站名就是裸名，没走别名改写）。**不留空**：
     留空会与"网关没上报"混淆；
   - 降级行（老网关整段缺 `realms`，连域都不知道）→ 显示 `—`。这里**不能**显示
     「同名」——那是"确认未改写"的断言，而我们并没有这个观测。
   另在「指标说明」卡片补一条 `出站模型` 说明。

**回归测试**：网关 `internal/server/metrics_test.go` 新增三条——
`TestMetricsOutModelsOnlyWhenRewritten`（别名请求输出、非别名与未路由都不输出）、
`TestMetricsOutModelsPerRealm`（落在域子条目：cn 侧 `kimi-k3-1` 输出、global 侧
与裸名同名不输出）、`TestMetricsOutModelsOmittedFromJSON`（非别名请求 JSON 里
不含 `out_models`，别名请求序列化为 `["al-phys"]`）。
`handler_alias_test.go` 新增 `TestChatAliasRecordsOutboundInStats`——端到端跑真实
handler，断言出站名落进 `/v1/stats`：赋值点（轮转循环内）与记账点（`chatStat.done()`）
任一处漏接线，单元测试仍会全绿而面板恒显示「同名」，故这条不能省。
面板侧 `internal/api/stats_test.go`（**新文件**）的 `TestStatsOutModelsPassthrough`
钉住 `out_models` 的**双向**存活（解码 + 编码），并断言未改写域不凭空多出该键。

**上游冲突面**：`client.go` 是新增字段；`StatsPage.tsx` 的按模型明细表格整块由第 11
条重写过，本条又插了一列（表头与 `StatRow` 各一处）——上游若重写该表格需两条一起重放；
`types.ts` 增字段。网关侧 `metrics.go` 是**本 fork 独有**的实现，`logging.go` /
`handler.go` 各加一处赋值。

## 15. 出站代理：两个 Transport 加 `Proxy` + ConfigPage 代理卡片

**动机**：网关部署在国内服务器时访问国外上游域（`www.workbuddy.ai`）需要代理。
网关侧新增了按域分档的 `proxy` 配置段（`config.json` 的 `proxy.cn` / `proxy.global`，
见根 README「出站代理」一节），面板也需要跟上：

- **面板自己的上游探测**（余额 / 模型 / 签到 / 成长任务）打的是上游域，与网关同一
  诉求。面板自身**没有** proxy 配置段——配置只有网关那一份 `config.json`，面板读的
  也是它，但面板后端并不解析 proxy 段（那是网关的 `cmd/server/config.go` 的事）。
  故面板侧沿用环境变量：面板与网关通常**同机部署**，`HTTP_PROXY` / `HTTPS_PROXY`
  一处生效即覆盖两边。需要按域精细分档时走网关侧（面板探测不参与选号，分不分域
  不影响出票）。
- **配置页需要能编辑 proxy 段**：否则用户得手动改 `config.json`，而面板的配置页
  是整份文档读写（未知键保留），代理段不渲染就只是「看不见但也不会丢」，用户没法
  在面板里配。

**改动**：

1. `internal/upstream/client.go`（**上游文件，需重放**）：`New()` 里的
   `http.Transport` 加 `Proxy: http.ProxyFromEnvironment`。零行为变更——Go 的
   `Transport.Proxy` 为零值时**不**读环境变量（默认直连），显式赋
   `ProxyFromEnvironment` 才是「认环境变量」。故这一行把面板从「无视代理」改成
   「跟环境变量走」。
2. `internal/gateway/client.go`（**上游文件，需重放**）：同上加一行。注意这个 client
   打的是**本机网关**（`127.0.0.1` / `localhost`），而 `http.ProxyFromEnvironment`
   对回环地址恒直连（Go 的 `httpproxy` 内置豁免 `localhost` / `127.0.0.0/8` / `::1`），
   故正常部署下这一行**不改变任何行为**；加上它只为「万一有人把网关地址配成远端
   域名」时口径一致。
3. `web/src/pages/ConfigPage.tsx`（**上游文件，需重放**）：在「上游超时」卡片之后
   新增「出站代理（按域分）」卡片——两个文本框（`proxy.cn` / `proxy.global`）。
   **说明文字放在 `.row` 之外整行**：两格共用一段 desc，若挂在其中一格内，
   `.row` 的 `align-items: flex-end` 会以该格底部对齐，把两格的输入框顶成垂直错位
   （第 13 条修过的同一个坑，见该节末）。

**未做**：面板的账号页**不加**代理字段。账号级代理只在 `auths/*.json` 顶层 `proxy`
键上承载，面板保存账号时靠 `preserve.go`（第 1 条）保证该键不丢——这正是第 1 条
那个「未知键保留」机制在本期的第二次派上用场。

> **注意**：`preserve.go` 只护住**面板自己的保存路径**。网关侧 `auth.SaveAtomic`
> 也是整份重建文档，所以网关这边另外做了两件事（都在根 module，不在 panel）：
> `Auth` 结构体增 `ProxyName` 字段（JSON 键仍是顶层 `proxy`）并 Parse/SaveAtomic
> 往返保留；`cmd/login` 重新登录落盘前先读回旧文件的 `proxy` / `device_token`
> （`carryPreservedFields`）。三处缺一处，账号级线路绑定就会在某条路径上被静默抹掉。

> **语义变更（本期）**：账号文件顶层 `proxy` 的值已从「直连代理地址」改为
> **线路名**（面板「出口线路」写入的形态），地址表在网关 `config.json` 的 `proxies`
> 段（线路名 → `http/https/socks5/socks5h` 地址），保留名 `__direct__` = 显式直连。
> 网关按线路名取独立 Transport（连接池按线路分开），名字查不到时**明确报错并回 503、
> 不罚账号**，绝不静默直连。面板的 `preserve.go` 按未知键原样保留，行为不变；面板
> 后端自身仍走环境变量代理（`panel/internal/upstream/client.go` 的
> `ProxyFromEnvironment`），不消费该字段——面板探测不参与选号，无需按线路隔离。

**上游冲突面**：两处 `Transport` 各一行（上游若重写这两个 `New()` 需重放）；
`ConfigPage.tsx` 新增一整张卡片（上游若改该文件的卡片排布需重放）。网关侧的
`proxy.go` / `transport.go` / `client.go` / `config.go` 全在根 module，不受 pull 影响。

## 纯新增、不会冲突的文件

| 文件 | 作用 |
|---|---|
| `host/host.go` | 面板装配包。**刻意不含 `internal` 段**，宿主导入合法（Go 的 internal 规则不允许根 module 直接 import `workbuddy2api-gui/internal/*`）。 |
| `internal/authstore/preserve.go` | 见上。 |
| `internal/authstore/preserve_test.go` | 见上。 |
| `internal/ops/aliasfile.go` | 见第 8 条。 |
| `internal/api/alias_test.go` | 见第 8 条。 |
| `internal/api/models_test.go` | 见第 8 条（`realms` 透传回归）。 |
| `internal/api/stats_test.go` | 见第 14 条（`out_models` 透传回归）。 |
| `internal/api/account_ledger_test.go` | 见第 9 条（账号台账透传回归）。 |
| `web/src/pages/ModelAliases.tsx` | 见第 8 条。 |
| `web/src/SuggestInput.tsx` | 见第 8 条（自绘候选下拉，替代被密码管理器抢占的 `<datalist>`）。 |
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
的网关语义已经改变**（统一调度改造，见 `internal/aliases/` 与
`internal/server/resolve_model.go`），两处口径现在**恰好一致**：

- `internal/server/handler.go` 的 `modelList` 已改为输出**裸名**（并集去重，
  同名条目只出现一次并带 `realms: ["cn","global"]` 标注可用域），不再输出
  `cn:` / `global:` 前缀条目。
- `internal/server/resolve_model.go` 的 `resolveModel`：裸名判为
  `RealmUnified`（全池），**按账号所属域自动路由**——正是上游文案描述的语义。
  `cn:` / `global:` 前缀仍然可用，作用改为「钉域」（只在该域账号中选号）。
  逃生门：`pool.unified_routing=false` 可让裸名退回旧语义（只路由 CN）。

因此在本 fork 上：

- 填**裸名**（`glm-5.3`）走全池调度：国内号与国际号都可能承接，按选中账号的域
  决定出站路径与鉴权头。这正是面板文案的说法。
- 填**带前缀名**（`global:xxx`）仍然**可用**，且比裸名更严格（钉死在 global 域）。
- 仅当两域模型名不一致时才需要「模型别名」页登记映射；否则裸名即可。

`web/src/pages/Models.tsx` 的说明文字无需改动其结论——它现在与本 fork 的行为一致了；
本次只补了两笔：说明 `cn:` / `global:` 前缀的作用已变为「钉域」（网关侧扩展），以及
提示「模型别名」页的用途（见第 8 条）。
