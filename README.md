# Magpie Intelligence Check · 定时 ModelTrace 分支

Magpie 插件，用于对指定的主力模型做质量探针，并在探针未通过或结果过期时拦截后续请求，等待自动检测或人工处理。

项目由两部分组成：

- **Magpie 插件**：`plugin/index.mjs` 负责启动后台、注册控制台入口和读取 Magpie 配置；`plugin/gate.middleware.js` 是 Magpie 请求门禁。
- **Go 后台**：`cmd/quality-check` 提供本机管理 API、SQLite 状态存储、日志监听、模型探针和临时远程控制台。管理页面嵌入在 Go 二进制中。

插件只拦截被明确加入监测的**完整模型 ID**。它不会替换 Magpie 的供应商配置、API 地址或 Key，也不会自动切换模型。

## 本分支：无人值守的定时 ModelTrace

本仓库 fork 自 `mu-zi-lee/magpie-intelligence-check` 0.3.0（本分支版本 0.4.0）。功能差异是：Go 后台内置一个**默认关闭**的定时器，按旧采集器 `magpie-modeltrace/service.py` 的节奏，对**显式勾选**的模型自动运行与手动按钮完全相同的 ModelTrace 三条指纹挑战，并在后台完成评分。门禁、牛奶题、评分页面、日志监听均未改动。

设计要点：

- 没有外部守护进程、新的鉴权令牌或新的管理服务；定时器是 Go 后台中的**一个 goroutine**，随插件宿主启动和退出。
- 触发走与手动按钮相同的探针路径，因此模型互斥、每轮 3 次原生探针、每日预算、结果持久化和评分界面都是唯一权威。
- 配置存放在既有的 SQLite `State` 中，通过既有的鉴权管理页面和 `/api/*` 修改；状态只读地出现在 `/api/state`。
- 调度器只读取 `targets` 来判断模型是否可检测，**不修改**目标状态、**不重写门禁文件**，牛奶题状态、放行租约和人工暂停不受影响。
- **后台评分**：`internal/check/fingerprint.go` 是 `web/modeltrace/fingerprint-core.js`（ModelTrace 统一全局稳健数字指纹）的 Go 移植，使用同一份嵌入的 `unified_bank.json`。调度不依赖浏览器是否打开。`testdata/fingerprint_golden.json` 由 `scripts/fingerprint-golden.mjs` 用确定性合成数字生成，Go 测试要求预测、概率和排名在 1e-9 内与原版 JS 一致。仓库不包含旧采集器归档中的原始探针回答。

### 节奏（沿用旧 `service.py`）

每个模型独立计数。一批 = 三条回答。只有**三条回答都可用**（每条至少 `max(80, ceil(0.55×要求个数))` 个有效数字）且最相似候选正好是期望模型时才是 **match**，连续一致次数 n 加 1；最相似候选是别的模型为 **mismatch**；两者都会结束 529 / 临时故障标记。

| 情形 | 下一次开始时间（自上一次尝试**开始**起算） |
| --- | --- |
| 连续一致 n = 0 / 1 / 2 / 3 / ≥4 | 15 / 30 / 60 / 120 / 240 分钟 |
| 最近一次 match 距今超过 **6 小时**（过期） | 连续一致按 0 计，即 15 分钟；之后再次 match 时计数在原值上继续加 1，与旧采集器一致 |
| mismatch | 连续一致归 0，15 分钟 |
| 未得出结果（拒答、截断、回答过短、4xx、仅 HTTP 200 无内容、批次不完整） | 连续一致归 0，并**暂停该模型的自动检测**，不显示下次时间；重启后仍保持暂停。手动运行一轮完整检测并得出 match / mismatch / 529 / 临时故障等明确结果后，恢复按上述节奏运行 |
| 确认的临时故障（网络错误、超时、HTTP 5xx，529 除外），自该批结束起 | 连续第 1 / 2 / ≥3 次：3 / 6 / 12 分钟 |
| 类型化的 HTTP 529，自该批结束起 | 连续第 1 / 2 / 3 / ≥4 次：1 / 2 / 4 / 6 小时；只有 match 或 mismatch 才结束该标记 |

529 或确认的临时故障出现后，该批剩余的探针**不再发送**（旧采集器同样不再向上游发请求）。错误文本从不参与判断，只看真实 HTTP 状态和类型化的传输错误。

状态按“尝试开始时间”落盘：**先预留后触发**，预留写入 SQLite 成功才发请求，所以重启、崩溃或触发失败都不会在同一时刻重复检测；预留后崩溃的一批显示为“服务重启，本次不重试”，计划从那次尝试的开始时间继续。服务停机期间错过的时间不补跑：恢复后只跑一次。

### 配置

打开控制台 → “模型指纹” → 展开“定时指纹检测”：

1. 勾选“启用定时检测”（默认关闭，且默认没有任何模型被勾选）；
2. 只勾选要检测的模型（必须已是监测目标，且能对应指纹库中的一个候选），设置每日最多定时次数；
3. 点击“保存定时设置”。保存后无需重启，定时器会立即按新配置重新计算下次时间。

等价的 API（需要既有的管理会话或控制令牌）：

```http
POST /api/trace-schedule
{"enabled": true, "models": {"magpie-252-ca704414/codex/gpt-6-astra": {"interval_minutes": 15, "daily_cap": 8}}}
```

| 项目 | 默认 | 范围 |
| --- | --- | --- |
| 总开关 | 关 | — |
| 模型白名单 | 空 | 最多 32 个，必须是已配置的监测目标 |
| 基线间隔 `interval_minutes` | 15 分钟 | 固定为 15；实际间隔随连续一致次数变化，其他值会被拒绝 |
| 每模型每日（UTC）定时次数 `daily_cap` | 8 次（= 24 次请求） | 1–24（稳态 4 小时节奏每天约 6 次） |
| 期望候选 `expect` | 模型 ID 最后一段 | 必须是指纹库候选；例如 `…/codex/gpt-6-astra` 对应 `gpt-6-astra` |

`daily_cap` 传 0 或省略时使用默认值；超出范围会被拒绝。原有的网关、日志路径和每日预算设置不会被这个接口修改，保存网关设置也不会清除定时配置。新加入定时的模型若从未有过尝试，**首次检测发生在保存时间之后的一个基线（15 分钟）**；已有状态（包括导入的旧状态）的模型保留自己的节奏，到点即测。

### 运行规则

- **安全跳过**：目标已删除、目标已关闭监测、网关未配置、该模型正在检测（含手动检测或牛奶题）、达到当日定时次数上限、预算不足。跳过后该模型退避一段时间再看（5 分钟；正在检测 1 分钟；次数上限和预算到下一个 UTC 日），原因显示在状态里，不会空转。
- **预算**：每轮 3 次请求，计入既有的每日每模型预算。定时检测只允许使用当日预算的**一半**（`每日上限 / 2`），另一半留给牛奶题门禁和手动检测。最终仍由探针路径的预算检查把关。
- **并发**：同一模型同一时刻只有一批；多个 tick 并发也只会预留一次。
- **唤醒次数**：关闭或白名单为空时不设置任何定时器，只等待配置变更；启用后睡眠到最近的到期时间，单次最长 10 分钟。不轮询网络。
- **手动批次**也会被评分并计入连续一致次数；它的开始时间同样推后下一次定时检测。

### 结果的含义

每条结果是**指纹结果**：这三条回答与指纹库中哪个候选最相似（闭集概率），**不是物理身份、账号或渠道的证明**。渠道系统提示词、协议转换和采样参数都会改变分布；库外模型也会被归到最相似的候选。

### 从旧采集器导入状态（显式、幂等）

旧 `service.py` 的状态（连续一致次数、最近一次开始时间）可以导入，使新插件接着旧节奏运行而不是从 15 分钟重新爬升。导入的是**事实**（次数、最近一致时间、最近尝试开始时间），到期时间由调度器按上面的规则推算，不写死任何未来时间。

```sh
# 1. 先停止旧的 service.py，否则它和新插件会在同一时刻各测一次。
# 2. 只读导出（旧的 history.sqlite 先被复制到临时目录再读取，旧目录不会被修改）：
bin/quality-check-darwin-arm64 export-legacy-seed \
  -models 'magpie-252-ca704414/codex/gpt-6-astra,magpie-252-8b686a70/codex/gpt-6-astra' \
  -status-md ~/.local/state/magpie-modeltrace/status.md \
  -out /absolute/path/seed.json
# 3. 预演（默认不写入），确认连续次数与到期时间：
bin/quality-check-darwin-arm64 import-trace-seed -data ~/.config/magpie-quality-check -seed /absolute/path/seed.json
# 4. 写入（需要先退出 Magpie / 插件后台，因为它持有数据目录锁；也可在后台运行时 POST /api/trace-seed）：
bin/quality-check-darwin-arm64 import-trace-seed -data ~/.config/magpie-quality-check -seed /absolute/path/seed.json -apply
# 5. 启动后再在控制台为这两个模型添加监测目标并启用定时检测。
```

- **以 SQLite 和运行日志为准**：连续次数从旧 `history` 表按旧规则（跳过 quota 行，遇到非 match 即止）计算，尝试开始时间取自 `attempts` 表；每一条构成连续次数的 match 都会核对对应的旧运行日志（已结束、3 个有效样本、预测与目标一致），并用本插件的评分重新计算一遍。日志与历史矛盾则导出失败；日志缺失或重新评分不一致只给出警告。旧的 `status.md` 仅作显示核对，不一致只给出警告。旧采集器还有未决的尝试时拒绝导出。
- **幂等**：同一份种子再次导入不改变任何状态（`already_imported`）；若该模型已有比种子更新的尝试（新插件已经跑过），种子被跳过（`skipped_newer_state`），不会覆盖。导入不启用定时检测，不创建监测目标，不发任何请求；每日计数保留已有值。
- **种子里没有邮箱**，只有 `sha256:` 前 8 位的审计引用。

2026-10-12 17:50 JST 前后用上述命令从真实旧状态导出并预演的结果（只是核对记录，不是配置）：`ca704414` 连续 8 次、当前间隔 4 小时、推算到期 19:47:15 JST；`8b686a70` 连续 34 次、4 小时、20:35:37 JST，与旧 `status.md` 一致。

**账号映射的限制**：新插件按**模型 ID**检测，看不到账号。导入时把模型 ID 中的 `magpie-252-<8 位十六进制>` 与旧历史里 `sha256(账号)` 的前 8 位匹配（必须恰好匹配一个账号），并假定 Magpie 把该模型 ID 路由到同一账号——插件无法验证这一点。旧采集器还用 `/v1/magpie/quotas` 检查 5 小时/7 天额度和账号唯一性后才发起检测，新插件**没有**这类额度检查，只有本地的每日预算与次数上限；额度耗尽时探针会得到 HTTP 错误并按上表退避。

### 兼容旧 widget 的 `snapshot.json`（显式 opt-in）

旧 `magpie-widget/proxy.py` 的 `/api/records` 只读 `~/.local/state/magpie-modeltrace/snapshot.json`，它由旧 `service.py` 在每次尝试后重写；旧采集器停止后没有人刷新，记录 6 小时后过期、widget 变空。新插件可以在**同时满足下列全部条件**时，从新的模型目标结果原子地刷新这个文件（以及显示用的 `status.md`），否则**一个字节都不碰**旧目录：

1. 种子导入时带了 `legacy_snapshot`（导出时加 `-legacy-snapshot`）——这是唯一的 opt-in；无关的新安装、不带该段的种子都不会写。
2. 定时检测已启用，且种子里映射的**每个**模型都在定时列表里、监测目标已启用、`expect` 与种子一致。关闭定时检测后写入停止，并交还旧锁，可随时回滚到旧采集器。
3. 旧采集器已停止：写入器持有旧目录的 `service.lock`（旧 `service.py` 终身持有的同一把 flock）。旧采集器仍在运行时，导入（含预演）直接失败；写入器活跃期间旧采集器无法再次启动（它拿不到锁会退出）。
4. 现有 `snapshot.json`（若存在）是普通文件（不是符号链接）、`version` 为 1，且其有效记录对应的账号全部在映射内（否则会丢账号，拒绝覆盖）。

```sh
# 1. 停止旧 service.py（含它的 launchd 条目，避免被拉起）。
# 2. 导出：种子里含有旧账号标识，只能写文件（0600），不能输出到 stdout：
bin/quality-check-darwin-arm64 export-legacy-seed -legacy-snapshot \
  -models 'magpie-252-ca704414/codex/gpt-6-astra,magpie-252-8b686a70/codex/gpt-6-astra' \
  -status-md ~/.local/state/magpie-modeltrace/status.md -out /absolute/path/seed.json
# 3. 预演 / 写入与上一节相同；含 legacy_snapshot 的种子文件权限必须是 0600，输出里不会出现账号。
# 4. 在控制台为这些模型添加监测目标并启用定时检测。
```

- **账号映射不模糊**：每个模型恰好对应一个旧 `(账号, 套餐)`；导入时校验 `sha256(账号)` 前 8 位 = 模型 ID 里的哈希 = 种子条目的 `account` 引用，同一旧账号不能被两个模型映射。`provider` 固定 `codex`、`source` 固定 `252`，`model` 是种子条目的 `expect`（如 `gpt-6-astra`）。
- **账号只存在于三处**：种子文件（0600）、插件私有状态库（0600，`legacy_compat`）、旧 `snapshot.json` 本身（旧 widget 以它为键）。控制台 `/api/state`、导入报告、错误信息、`status.md`（沿用旧的遮罩格式）都不含完整账号；`snapshot.json` 不含提示词、回答或任何凭据。
- **字段与语义沿用旧 `snapshot()`**：`version`、`generatedAt`、`records`（`provider/source/account/plan/model/mismatch/at/expiresAt/lastAttemptFailed/lastAttemptAt/lastAttemptStatus/degraded/degradedReason/degradedAt/retryAfter`）、`historyCount`。最近一次评分结果（match/mismatch）保留 6 小时；其后的失败尝试体现在 `lastAttempt*`；未解除的 529 没有 TTL，`expiresAt` 是旧采集器同样使用的有限固定值 `253402300799`，`degradedAt` 为最近一次 529，`retryAfter` 为其后 1/2/4/6 小时，直到一个完整的 match/mismatch 批次解除。种子里会带上“被后来的失败盖住的最近评分结果”（`scored_at`/`scored_prediction`），所以降级时已有的新鲜记录不会丢。
- **刷新节奏**：每个批次分类后、导入/改定时后立即写；之后每分钟检查一次，内容变化或距上次写入满 5 分钟时重写（`generatedAt` 更新、过期记录被剔除）。写入是同目录临时文件 + fsync + 重命名，权限 0600。

**与旧采集器不完全等价之处（已知）**：

- `historyCount` = 导出时旧 `history` 表总行数 + 导入后新插件已分类的批次数。旧表里的 `quota` 行等新插件没有对应物，之后的计数因此不可能逐行一致。
- 连续 529 的次数按新调度器的口径（只有评分批次才清零 529 连续次数），旧采集器遇到“529、其他失败、529”时只数最后一个 529；`retryAfter` 因此可能比旧值更长。
- 旧 widget 看到的 `at` 是新插件**分类批次的时刻**，旧的是历史行写入时刻；`lastAttemptStatus` 只有 `match` / `mismatch` / `failed` / `degraded`（没有旧表里的 `quota`）。
- 旧采集器每分钟空转时也会刷新 `status.md`；新插件只在写 `snapshot.json` 的同时写它。
- 写入器停止（定时关闭、插件退出）后文件不再更新：普通记录 6 小时后自然过期，**未解除的 529 记录不会过期**（与旧采集器停止后相同）。
- 旧版二进制读取这份状态可以，但它的下一次保存会丢掉 `legacy_compat`（与 `trace_schedule` 同理），写入器随之停止。

### 限制

- 只提供**模型级**行为记录，不会自动封锁模型，也不改变门禁；检测结果仅供人工查看。
- 只有 Magpie 宿主进程运行时才会检测；宿主退出期间不补跑。
- 每日计数和预算以 UTC 日期为准，与现有预算显示一致。
- 未得出结果的批次（非 529、非临时故障）与旧采集器的 `pending_unresolved` 一致：该模型暂停自动检测，界面显示暂停原因而非下次时间，直到手动一轮完整检测给出明确结果。导入旧状态时，最近结果为“未得出结果”的模型同样处于暂停。
- 导入旧状态时，只有旧运行日志本身证明（摘要与最后一条推理终止记录都是 HTTP 529）的 529 才计为降级；仅凭历史记录里的 detail 不被信任，按“未得出结果”处理（暂停）。
- 状态数据多了 `trace_schedule` 字段。旧版二进制可以读取新数据，但它的下一次保存会丢弃这个字段（历史结果与目标不受影响）。

### 构建、安装与回滚

本仓库包含五个平台的预编译二进制，插件会按系统和架构自动选择。安装时无需 Go 或 Node.js。

重新构建（可选，Go 1.26）：

```sh
cd /absolute/path/to/magpie-intelligence-check
go test -race ./... && go vet ./...
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags='-s -w' \
  -o bin/quality-check-darwin-arm64 ./cmd/quality-check
```

从 Magpie 安装框添加本 fork：`github:didogakki/magpie-intelligence-check`。升级现有本地安装时，先备份数据目录，再移除旧安装，添加本 fork；不要同时启用两个共用 `dataDir` 的插件。

本地源码安装：

1. 退出 Magpie，备份数据目录：`cp -R ~/.config/magpie-quality-check ~/magpie-quality-check.bak`（含 `check.db`、`-wal`、`-shm`）；
2. 停用或移除已安装的旧插件（命令以 `magpie plugin --help` 为准），再 `magpie plugin add /absolute/path/to/magpie-intelligence-check`（目录需保持固定且可写）。两个插件默认共用同一个 `dataDir`，由 `service.lock` 保证同一时间只有一个后台，所以不能同时启用；共用数据目录正是为了保留现有目标、预算和历史；
3. 启动 Magpie，在“模型指纹”页按上文配置定时检测。

回滚：退出 Magpie → 移除本 fork → 重新添加上游 `mu-zi-lee/magpie-intelligence-check` → 启动。数据可原样沿用；若要恢复到切换前的状态，退出后用备份目录替换数据目录。紧急停止定时检测不需要回滚：在控制台关闭总开关即可。

验证（本分支新增 `schedule_test.go`、`fingerprint_test.go`、`seed_test.go`、`legacy_test.go`，使用本地 httptest 夹具和归档回答，不产生真实推理请求）：

```sh
go test -race ./...
node --test plugin/*.test.mjs
```

## 工作流程

```text
客户端 -> Magpie -> quality-check 门禁 -> 上游模型
                         |
                         +-- 未启用监测 ----------------> 直接通过
                         +-- 检测通过且未过期 ----------> 直接通过
                         +-- 未检测或检测已过期 --------> 返回 403
                                                          |
                                                          +-- 日志触发后台探针
                                                          +-- 控制台手动检测
```

首次使用或检测结果超过 15 分钟后，门禁会返回 `403`，原业务请求不会发往上游。后台完成检测后，客户端需要重新发送原请求；是否自动重试由客户端决定。

同一模型的并发触发会合并为一轮检测。检测通过结果从完成时间起有效 15 分钟；闲置期间不会主动补跑检测。

## 安装

安装、构建和回滚步骤见上文“本分支”一节。下面的原版说明（GitHub 安装框、发行包）对应上游 `mu-zi-lee/magpie-intelligence-check`，**不包含**定时 ModelTrace。

### 上游安装方式（仅供参考）

#### 从 Magpie 的安装框添加

在“已有想装的？”输入框填写：

```text
github:mu-zi-lee/magpie-intelligence-check
```

仓库包含五个平台的 Go 二进制，插件会按 Magpie 所在机器的系统和架构选择。
供应商显示名为 **Intelligence Check**。更新后请重新安装插件并重新打开管理入口。

#### 使用发行包

发行包在 `dist/` 中，按平台提供完整插件目录和已编译的 Go 二进制：

| 平台 | 文件 |
| --- | --- |
| Linux x86_64 | `magpie-intelligence-check-linux-amd64.tgz` |
| Linux ARM64 | `magpie-intelligence-check-linux-arm64.tgz` |
| macOS Intel | `magpie-intelligence-check-darwin-amd64.tgz` |
| macOS Apple Silicon | `magpie-intelligence-check-darwin-arm64.tgz` |
| Windows x86_64 | `magpie-intelligence-check-windows-amd64.tgz` |

解压后，把解压出的**完整目录**添加到 Magpie：

```sh
tar -xzf magpie-intelligence-check-darwin-arm64.tgz
magpie plugin add /absolute/path/to/magpie-intelligence-check-darwin-arm64
```

安装目录需要保持固定并可写。插件会在其中读取 JavaScript 门禁文件，并由 Go 后台原子更新该文件。

发行包已经包含对应平台的 Go 二进制，运行机器不需要安装 Go 或 Node.js。

#### 从源码构建

需要 Go `1.26` 工具链：

```sh
go build -o bin/quality-check ./cmd/quality-check
magpie plugin add /absolute/path/to/magpie-intelligence-check
```

开发目录需要包含 `plugin/`、`bin/quality-check`、`package.json` 和 `third_party/`。Magpie 主进程（桌面应用、`magpie web` 或 `magpie serve`）必须保持运行；插件后台跟随宿主进程生命周期。

## 首次配置

1. 在 Magpie 插件入口打开“本机质量检测控制台”。
2. 点击右上角设置图标，在“连接与预算”中确认网关 URL、Key 和日志路径，保存后会验证连接。
3. 点击“刷新模型”，选择完整模型 ID，例如 `provider/model`，添加监测目标。
4. 先运行一次“牛奶推理”确认网关和探针可用。
5. 从客户端发送一次该模型的业务请求，确认门禁、日志触发和检测流程正常。

插件会优先通过以下方式发现本机网关：

- `MAGPIE_ADDR` 环境变量；
- `$XDG_CONFIG_HOME/magpie/settings.json` 中的监听端口；
- 默认地址 `http://127.0.0.1:3425`。

发现通过回环地址访问网关，并用 Key `magpie` 请求 `/v1/models` 验证连接。`0.0.0.0:端口`、`[::]:端口` 和 `:端口` 会转换为对应的本机地址。启动时在后台发现，不阻塞 Magpie 初始化；网关尚未就绪时每 5 秒重试。远程地址或其他 Key 需要在控制台手动填写。已有配置不会被自动覆盖。

如果仍提示“请配置 Magpie 网关 URL 和 Key”，点击连接设置，填写 **Magpie 所在机器**可访问的网关地址，例如 `http://127.0.0.1:3425`，以及网关 Key（本机默认 `magpie`），再保存。它不是 Cloudflare 控制台域名，也不是上游供应商地址。Docker 中网关不在同一容器时，请填写容器可访问的实际地址与 Key。

控制台提供模型监测、质量题库、模型指纹和检测记录四个视图；支持暗亮主题、手机操作、记录筛选、历史指纹批次和原始回答下载。自动刷新会保留未保存的设置与已展开的回答。

只需要填写 Magpie 网关的 URL 和 Key，不需要重复填写各上游供应商凭据。

### 日志源

自动触发依赖 Magpie 日志。门禁在拦截请求时写入一条不包含对话内容和网关 Key 的标记，Go 后台从日志中读取新标记并启动牛奶题探针。

例如：

```sh
mkdir -p "$HOME/.local/state/magpie"
chmod 700 "$HOME/.local/state/magpie"
umask 077
magpie serve 2>> "$HOME/.local/state/magpie/gateway.log"
```

然后把这个**绝对路径**填入控制台。已有 systemd 或其他服务可以直接把标准错误重定向到可读文件，不需要启动第二个 Magpie 实例。

后台每秒检查日志，并支持文件截断、重命名和轮转。首次打开日志时从文件末尾开始，不会补跑历史内容。

没有可读日志时，手动检测仍然可用，但自动触发不可用；门禁仍会阻止未验证目标。日志文件可能包含其他 Magpie 敏感信息，请限制其权限。

## 远程控制台

选择“打开临时远程控制台（Cloudflare）”后，插件会启动 Cloudflare Quick Tunnel，把本机管理页面临时暴露为 HTTPS 地址：

- 一次性 `/open/...` 链接有效 10 分钟；
- 浏览器会话最长 30 分钟；
- 临时通道最长 30 分钟，也可以在页面中提前结束；
- 关闭通道会撤销远程会话，但不会解除模型暂停，也不会停止检测后台；
- 只开放管理页面，不开放 Magpie 模型 API，网关 Key 不返回浏览器。

插件优先使用 PATH 中已有的官方 `cloudflared`。找不到时，默认下载并校验固定版本 `2026.10.0`，缓存到数据目录的 `runtime/`，不需要 root，也不会修改系统 PATH。自动下载需要访问 GitHub 发行文件和 Cloudflare 隧道服务；可通过 `cloudflaredPath` 指定已有客户端，或关闭自动下载。

远程入口会先等待公网 `/health` 就绪，最长 60 秒，再返回管理链接。
如果出现 `502`，本轮就绪检查失败会关闭通道；检查网络后从插件重新打开。
管理链接从外部页面打开时，使用 `HttpOnly`、`SameSite=Lax` 会话 Cookie 完成重定向；
会话过期后需要从插件取得新链接。

不要只打开临时域名首页，必须使用插件返回的完整 `/open/...` 链接。不要把管理端口直接绑定到公网。

## 检测规则

内置 7 道题：

| ID | 题目 | 是否参与自动门禁 |
| --- | --- | --- |
| `milk` | 牛奶推理 | 是 |
| `repeat` | 逐字复述 | 否，人工评价 |
| `japan` | 日本首相 | 否，人工评价 |
| `person` | 人物知识 | 否，人工评价 |
| `html` | HTML/SVG 动画 | 否，人工评价 |
| `svg` | SVG 创作 | 否，人工评价 |
| `map` | 世界海陆地图 | 否，人工评价 |

提示词正文按源码原样发送，保留字符、标点、空格和空行。题目文本有 SHA-256 回归测试；代码围栏、语言标记和评价说明不会追加到发送正文。

自动门禁只使用 `milk`：最终答案为 `46.24`，或最后一个带 `inches`/`inch`/`英寸` 单位的数字为 `46.24` 时通过。首次答案异常会独立复测一次；两次都未通过则进入“需要人工确认”。

其他题目会保存原始回答和 token 用量，在质量题库页面直接展示，也可选择历史回答、复制或下载。失败与截断会显示原因，并保留已返回的回答正文。用户可保存“待确认 / 确认通过 / 标记问题”与审核备注；结论记录在对应回答上，不更改自动评分或模型门禁。自动辅助检查展示字符数、逐字复述覆盖率或代码标记等可验证线索，不识别模型身份，也不替代人工审核。后台不会执行模型生成的 HTML、SVG 或 Python。

### 预算与超时

- 默认每个模型每天最多 120 次探针请求，可在控制台设置为 1–10000；
- 每轮自动检测最多调用 2 次；
- 单次请求 `max_tokens` 为 8192；
- 单轮总超时为 180 秒；
- 网关调用失败、超时、空回答、截断、无效响应或预算耗尽会标记为“检测不可用”；
- 检测不可用后自动冷却 15 分钟，手动检测可以立即发起。

探针请求使用 Magpie 的 Chat Completions 接口，并把模型改写为内部别名 `quality-probe/<随机别名>/<完整模型 ID>`。门禁会放行这个内部别名，避免探针再次触发门禁。

这些题目只是行为与能力线索，不是模型身份认证、供应商认证或通用智商排名。单题失败不能证明模型被替换；知识题有时效性，复述指纹也可能随版本变化。

### 题库反馈与更新

质量测试支持流式正文（网关返回普通 JSON 时仍兼容）、等待时间和取消；取消或连接中断保留已返回正文，生成中不会执行预览。HTML 可切换代码块、选择手机/平板/桌面宽度、页内放大，并可选择明确标注的 HTML + CSS/JS 组合结果；原始回答不修改。离开题库会停止预览。历史支持题目、模型、结果、审核状态筛选，双回答对比与筛选结果 JSON 导出。清理需确认，检测期间不可清理；只移除回答与审核，保留已消耗预算计数，不更改模型门禁。

HTML 预览额外移除声明式自动刷新、外链导航、表单目标和 base 标签，并使用不含同源、顶层导航或弹窗权限的沙箱。允许任意内联脚本时，浏览器不能可靠禁止 iframe 自身的脚本导航，因而这不是完整的离线执行环境；只预览可信回答，不要在预览中输入敏感信息。网络 API、外部资源和管理页访问限制仍生效。

SVG 题中的完整 SVG 可直接静态预览，不执行脚本、外部资源或动画。HTML 题可在隔离的 iframe 中实时预览模型生成的页面、内联 JavaScript、Canvas 和动画，并可停止或重新加载；轮询不会重启动画。预览不开放管理页面、Cookie 或存储访问权限，禁止网络 API 请求、外部脚本、嵌套页面、表单提交和弹窗。依赖 CDN 或外部图片的回答可能无法完整显示；不可信脚本仍可能消耗浏览器资源，必要时停止预览或关闭页面。原始回答保持不变，可查看、复制与下载。关闭模型监测只关闭门禁与自动触发，仍可手动运行质量题和指纹挑战；删除目标后测试选择器同步移除，再添加后同步更新。

欢迎实际测试后反馈：哪些题目现在仍然好用，哪些已经失效、答案有争议，或容易误判？现有题库不保证对所有模型版本、渠道和系统提示词都有效，请结合页面中的原始回答自行确认。

如果你有**更新、可用的测试题**，或发现更适合推理、翻译、知识、复述等能力的题目，请[提交题库反馈 Issue](https://github.com/mu-zi-lee/magpie-intelligence-check/issues/new?template=question-bank.yml)。维护者会审核后手动添加或更新，不会自动把社区题目纳入门禁。

提交时请提供：

- 题目原文与来源；原创题目请注明。
- 测试的模型、版本或日期、渠道，以及是否使用额外系统提示词。
- 原始回答、预期答案或人工评价标准。
- 为什么认为它有效；有多次测试或不同模型的对照结果更好。
- 已知限制，例如知识时效性、不同合理答案或可能的误判。

请勿提交 API Key、管理链接、私人对话或其他敏感信息。无需为反馈专门重复付费测试，已有的匿名化结果即可。

### ModelTrace 模型指纹

控制台提供独立的 **ModelTrace 模型指纹** 检测。选择已启用监测的模型，点击“运行三条指纹挑战”，后台通过同一 Magpie 网关发送三次独立数字选择请求。每条要求生成 292–332 个 1–355 范围内的整数。

- 每轮预留 3 次当日预算，不足时不发请求；没有自动复测；
- 每条请求最多 180 秒，三条顺序执行；关闭浏览器不会中断后台采集；
- 保存每条挑战原文、原始回答、token 用量和批次，刷新页面后仍可查看；
- 三条回答都有效才展示归因；拒答、截断、调用失败或有效数字不足不会被当作模型异常；
- 浏览器本地使用 ModelTrace 原版评分算法及固定候选库，展示候选模型排名和家族概率；
- 不改变牛奶题的通过状态、人工暂停或临时放行。

当前候选库含 GPT、Claude 两个家族的 18 个模型。概率是库内候选的闭集概率：
即使真实模型不在库里，也会归到某个最相似候选，因此不能据此认证身份或自动封锁模型。
渠道系统提示词和协议转换也可能改变输出分布。

来源为 `xqy2006/ModelTrace`，固定提交和资源校验值见 `third_party/modeltrace/README.md`；
采用 MIT 许可，评分代码与候选库嵌入 Go 二进制，无需 Python、外部评分服务或运行时下载。

## 状态与人工操作

控制台中的目标状态包括：

- **未检测**：目标已启用，但没有有效检测结果；
- **检测中**：后台正在运行自动检测；
- **通过**：结果有效期内允许请求；
- **需要人工确认**：自动题复测仍异常，继续拦截；
- **检测不可用**：网关、超时或预算等原因导致本轮无法完成；
- **监测关闭**：目标不再受门禁控制。

可执行操作：

| 操作 | 作用 |
| --- | --- |
| 重新检测 | 立即运行所选题目，计入预算 |
| 放行 15 分钟 | 暂时允许该目标请求，保留异常记录 |
| 暂停 | 设置为人工确认状态并持续拦截 |
| 关闭监测 | 关闭该目标的检测和门禁 |
| 删除监测目标 | 从列表移除并解除门禁，保留历史回答和审核；检测中不可删除 |

自动门禁返回的是实际 API `403`，消息包含目标、原因和事件编号，例如：

```text
[MAGPIE_QUALITY_REVIEW_REQUIRED] 需要人工确认。
本次请求未发往上游。
```

插件只保证匹配目标的模型请求被拦截，不控制客户端已经开始的本地工具操作或整个任务流程。

## 插件选项

可以在 Magpie 插件配置中传入：

```json
{
  "dataDir": "/srv/magpie-quality/data",
  "gatewayURL": "http://127.0.0.1:3425",
  "gatewayKey": "magpie",
  "logPath": "/srv/magpie/gateway.log",
  "binaryPath": "/srv/magpie-quality/bin/quality-check",
  "cloudflaredPath": "/usr/local/bin/cloudflared",
  "autoInstallCloudflared": true
}
```

字段说明：

- `dataDir`：SQLite、门禁状态和 Cloudflare 缓存目录。默认 `~/.config/magpie-quality-check`；
- `gatewayURL`、`gatewayKey`、`logPath`：用于首次自动连接配置，不覆盖控制台已有设置；
- `binaryPath`：指定 Go 后台二进制；
- `cloudflaredPath`：指定官方 `cloudflared`；
- `autoInstallCloudflared`：设为 `false` 时禁止自动下载 Cloudflare 客户端。

数据目录会以私有权限创建，SQLite 中保存网关凭据、历史结果和目标状态。停用、退出或重启不会清除数据；卸载插件也不会自动删除数据目录，请按需手动备份或清理。

同一个 `dataDir` 只允许一个检测后台运行。多个 Magpie 实例必须使用独立的插件目录和 `dataDir`。

## 限制与兼容性

- 依赖 Magpie 的插件系统、中间件热加载和可读日志；
- 门禁按最终请求中的完整模型 ID 判断，自动路由别名不应当当作目标 ID；
- 由于门禁文件热加载存在传播窗口，Magpie 中间件加载失败时默认放行；这不是强安全隔离；
- 网关 Key 需要有权限调用 `/v1/models` 和 `/v1/chat/completions`，并允许探针模型别名；
- 当前探针使用非流式 Chat Completions，由 Magpie 负责上游协议转换；
- 发行脚本生成 Linux、macOS 和 Windows 的交叉编译包，实际部署仍需检查网络、文件权限、日志路径和 Magpie 版本；
- Cloudflare Quick Tunnel 是临时通道，不保证公网可用性或稳定性。

## 开发与验收

运行 Go 测试和插件测试：

```sh
go test -race ./...
go vet ./...
node --test plugin/*.test.mjs
```

构建发行包和校验清单：

```sh
node scripts/build-release.mjs
node scripts/package-check.mjs
```

可选的集成检查：

```sh
MAGPIE_BINARY=/absolute/path/to/magpie node scripts/magpie-integration.mjs
MAGPIE_BINARY=/absolute/path/to/magpie MAGPIE_BUN=/absolute/path/to/bun node scripts/gateway-integration.mjs
MAGPIE_BINARY=/absolute/path/to/magpie node scripts/install-check.mjs
node scripts/plugin-check.mjs
node scripts/client-check.mjs
node scripts/cloudflare-check.mjs
node scripts/browser-check.mjs
node scripts/auth-browser-check.mjs
```

其中：

- `magpie-integration.mjs` 验证真实 Magpie 网关中的门禁、探针别名和日志触发；
- `gateway-integration.mjs` 验证真实 Magpie 管理页面加载完整插件后自动发现、验证并保存网关；需要 Node.js 24，`MAGPIE_BUN` 指向已有 Bun，测试使用独立的临时配置；
- `install-check.mjs` 验证发行包解压、插件注册和宿主退出清理；
- `client-check.mjs` 验证客户端能看到 `MAGPIE_QUALITY_REVIEW_REQUIRED` API 错误；
- `cloudflare-check.mjs` 验证临时 HTTPS 入口、会话交换和关闭；
- `browser-check.mjs` 需要 Playwright，可通过 `PLAYWRIGHT_MODULE` 指定模块位置。
- `auth-browser-check.mjs` 使用真实浏览器验证从外部页面打开管理链接、票据重定向和会话刷新。

## 目录结构

```text
cmd/quality-check/       Go 后台入口
internal/check/          状态、探针、HTTP 控制台和日志监听
internal/check/web/      嵌入 Go 二进制的管理页面
internal/check/schedule.go 定时 ModelTrace 调度器（本分支新增）
plugin/index.mjs         Magpie 插件入口
plugin/gate.middleware.js 请求门禁（运行时由后台原子更新）
plugin/cloudflared-runtime.mjs  Cloudflare 客户端发现、下载和校验
scripts/                 构建、集成和客户端验收脚本
third_party/cloudflared/ Cloudflare 客户端许可文件
```

## 许可

本项目采用 MIT License。Cloudflare 客户端相关许可随包保留在 `third_party/cloudflared/LICENSE`；ModelTrace 许可保留在 `third_party/modeltrace/LICENSE`。
页面内置 Space Grotesk、Space Mono 字体和 Lucide 图标，无需运行时 CDN；许可分别保留在 `third_party/fonts/` 和 `third_party/lucide/LICENSE`。
