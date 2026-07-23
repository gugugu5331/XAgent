# Lifecycle Hook System Checklist

> 本清单只描述可观察的验收结果，不替代实现任务。代码尚未实现或尚未取得实际证据时一律保持未勾选；每项只有在记录命令输出、测试结果或真实 TUI 观察后才能勾选。

## 配置发现、严格校验与启动边界

- [x] C001：只从 `~/.config/xagent/hooks.yaml` 与 `<project>/.xagent/hooks.yaml` 两个固定位置加载 Hook；任一文件缺失均可启动，两者都存在时按用户文件、项目文件的顺序累加而非覆盖。（验证：分别运行无文件、仅用户、仅项目、双文件四组启动/加载测试；期望规则集合和触发顺序逐组准确。）

- [x] C002：合法配置必须使用 `version: 1` 和严格的 `hooks` 列表；未知版本/字段/事件/action、一个规则多个 action、错误字段类型、第二份 YAML document、重复或非标量 key、anchor/alias/merge key/custom tag 均拒绝启动。（验证：运行严格结构与 schema 表驱动测试；期望每个非法样本失败，合法四 action 样本通过。）

- [x] C003：YAML、schema、正则、模板、路径和组合校验错误都能定位到规范文件、行列、字段路径；规则级错误还包含从 1 开始的规则序号，且错误内容经过脱敏。（验证：向不同层级注入语法和语义错误及 secret canary；期望定位字段完整且输出不含 canary 原值。）

- [x] C004：Hook 严格加载和集中校验发生在 Store、Provider、工具注册、MCP、Skill、网络和进程等副作用之前；非法现存配置使进程在 TUI 接受输入前退出。（验证：运行带 recording factory 的启动测试；期望坏配置时所有后续 factory、网络、Provider、MCP 和进程计数均为 0。）

- [x] C005：每条规则以规范来源和声明序号形成稳定身份，并按用户文件到项目文件、各文件声明顺序取得唯一有效序号；结果不受目录遍历、map 顺序或文件创建时间影响。（验证：重复构造相同配置并扰动输入容器顺序；期望来源、规则序号和有效顺序逐次一致。）

- [x] C006：配置在进程启动时形成不可变快照；运行中修改、创建或删除 Hook 文件不会改变当前规则、once 或 Prompt 状态，重启后才生效。（验证：启动后改动两个固定文件并再次触发；期望本进程仍使用旧快照，新进程使用新配置。）

- [x] C007：版本 1 恰好接受 `system_start/system_stop`、`session_start/session_end`、`turn_start/turn_end`、`message_before/message_after`、`tool_before/tool_after`、`compact_before/compact_after` 这 12 个事件，以及 `command/http/prompt/subagent` 四类动作；各动作只接受自己的严格字段集合。（验证：逐一加载 12 个事件的合法代表样本及未知枚举/交叉字段样本；期望合法项通过、越界项启动失败。）

- [x] C008：`once=false`、`async=false`、Command timeout=30s、HTTP timeout=10s、Prompt scope=turn、HTTP method=POST 和 `send_event=true` 的默认值准确；timeout 只适用于 Command/HTTP/SubAgent，必须为 Go duration 且 `0 < timeout <= 10m`，`tool_before` 与所有 Prompt 禁止异步，其他不兼容组合在启动期拒绝。（验证：运行 presence、默认值、边界值与非法组合矩阵；期望省略值和显式 false 可区分，所有禁止组合无法启动。）

- [x] C009：单文件 256 KiB、每文件 256 条规则、每规则 32 个条件均有 limit/limit+1 证据；所有字节限制按 UTF-8 字节而非字符数计算。（验证：运行配置资源边界测试；期望恰好上限通过，超过 1 个单位时启动失败且无部分规则发布。）

## 生命周期事件与统一 EventContext

- [x] C010：`system_start` 只在完整运行时装配后触发一次，`system_stop` 只在成功完成 start 后触发一次；构造中途失败、重复或并发关闭不会伪造或重复系统事件。（验证：运行正常、start 前失败、重复 start/close 和并发 close 测试；期望事件计数和顺序精确。）

- [x] C011：新建与恢复 Conversation 分别产生带 `new`、`resumed` 的 `session_start`；切换和退出在最终保存后产生带准确 reason 的 `session_end`，随后才清理 session 状态。（验证：记录新建、恢复、切换、退出的 save/event/cleanup 时间线；期望顺序和载荷符合定义。）

- [x] C012：会话切换是事务式的：候选加载失败时旧 Conversation、模式、Skill 和 Hook Prompt 完全不动；成功时等待活动 run 收尾，再保存旧会话、结束旧 session、清理并启动新 session。（验证：分别制造候选失败、保存失败和成功切换；期望失败不半切换，成功顺序唯一且保存失败仍安全收尾。）

- [x] C013：输入、mode、Conversation 或 profile 验证失败不产生 Turn；验证成功后，一个普通用户请求无论包含多少 Provider iteration 都只有一组 `turn_start`/`turn_end`，结束状态准确映射为 `completed`、`error`、`canceled` 或 `max_iterations`，terminal UI 事件晚于 turn_end。（验证：对前置失败、四种终态和多 iteration 运行集成测试；期望事件有无、计数和顺序准确。）

- [x] C014：普通用户消息的可观察顺序为 turn_start → message_before(user) → Conversation append → message_after(user) → Agent Loop。（验证：用 recorder 包围一次请求；期望五个边界严格按序且 before/after 复用同一消息身份。）

- [x] C015：只有实际写入 Conversation 的完整 assistant 消息产生一组 before/append/after；多次 Provider iteration 中的每条完整 assistant 消息各触发一次。（验证：运行含中间 assistant/tool iteration 的会话；期望完整 append 数与 assistant 事件组数相等。）

- [x] C016：流式 delta、thinking、tool_call、tool_result、压缩边界、Memory 内部消息、摘要和 UI/历史 bridge copy 均不产生 Message Hook。（验证：为每类内部记录设置唯一 canary 并运行综合会话；期望 Message recorder 中零命中。）

- [x] C017：内置工具、MCP 工具和系统工具 `load_skill` 在通过注册与前置安全检查后都恰好产生一次 `tool_before`；实际进入 handler 后无论 success、error 或 timeout 都恰好产生一次对应 `tool_after`。（验证：运行 3×3 正向工具矩阵；期望 before/after 次数和状态精确。）

- [x] C018：空白工具参数规范化为 `{}`；未知/伪造或 Skill 隐藏工具，以及非空的 scalar/array、trailing/重复顶层值或非法 JSON 参数，都在 Hook 前拒绝且无 before/after。Plan、Normalize/硬安全、Hook deny、普通权限拒绝和用户取消均无 after，只有已进入 handler 的工具专属或 `load_skill` 业务参数错误才有 after。（验证：运行完整结构预检和 pre-handler 拒绝矩阵；期望空白正例与各负例的事件/handler 计数在准确边界停止。）

- [x] C019：`compact_before`/`compact_after` 只包围真实压缩 attempt；before 含 reason 和压缩前统计，after 复用同一 token/前统计并附 success/error 及可用后统计，非真实 auto 检查不触发而 manual 即使无变化也触发。（验证：运行 auto-noop、auto-summary、manual-nochange、success 和 error 矩阵；期望事件边界和统计准确。）

- [x] C020：本地 `/clear`、`/plan`、`/do`、`/diagnostics` 不产生虚假 turn/message；`/compact` 只产生真实 compact 事件，会话命令造成的真实切换只产生 session 生命周期。（验证：逐条执行本地命令并比较事件 recorder；期望只有真实副作用对应的生命周期出现。）

- [x] C021：独立 Skill 使用 `execution.kind=isolated_skill`，产生自己的 turn/message/tool/compact 事件但不新增 session；direct 与 Agent 触发两种入口均只结束一次 child turn。（验证：运行两种 isolated 入口；期望 child 身份独立、session 计数不变且生命周期不重复。）

- [x] C022：每个事件上下文至少包含 schema_version、事件名、进程内单调 sequence、RFC3339Nano 时间和绝对 clean 项目根；存在 Agent Run 时才包含 execution ID/kind/mode，Do Mode 映射为 `default`，不适用对象完全省略而非填空对象。（验证：固定 clock/ID 构建 12 种事件并解析规范 JSON；期望必选、可选和省略字段逐项匹配。）

- [x] C023：session 的 state/end_reason、turn 的 status/error、message 的 id/role/content、tool 的 call/name/arguments/status/duration/result 和 compact 的 reason/before/status/after/error 只在规定阶段出现；工具参数保持无精度损失的 JSON 标量/对象/数组且不依赖对象键序，工具结果只暴露已限长脱敏的模型可见内容与错误。（验证：用 12 种 payload、大数、嵌套参数及安全化结果 round-trip；期望字段目录、类型和阶段逐项准确。）

- [x] C024：同一次触发的条件、Command stdin、默认 HTTP body 和 Prompt 模板读取同一个不可变快照；外部对象随后修改不影响它，所有 dispatch 以 sequence 提供全序且 EventContext 不自动进入 Conversation、模型上下文或诊断。（验证：在各消费者之间变异原输入并并发触发；期望四方值一致、sequence 唯一递增且非目标存储中无 payload。）

## 条件表达式与严格模板

- [x] C025：省略 `if` 的规则无条件触发；存在 `if` 时只能选择一个非空 `all` 或一个非空 `any`，不允许嵌套、空列表、同时出现或混用。（验证：运行无条件、all/any 正反例和非法结构表；期望选择语义准确、非法结构启动失败。）

- [x] C026：`exact` 对字符串、布尔和有限十进制数字执行类型化比较；数值 `1` 与 `1.0` 相等而字符串 `"1"` 不相等，null、NaN/Inf、时间、对象和数组不能直接匹配。（验证：运行 JSON/YAML 数字、布尔、字符串及非法/复合值矩阵；期望无精度损失且跨类型不误命中。）

- [x] C027：glob 与 regex 只匹配字符串、区分大小写、不隐式 trim，并对包括尾换行在内的完整值匹配；regex 子串必须显式写 `.*` 且非法表达式在启动时失败。（验证：运行大小写、空白、多行、尾换行、全值/子串正反例；期望结果逐项符合定义。）

- [x] C028：`negate` 只在字段存在且类型合法后反转；字段缺失、路径不存在或类型不支持时始终为 false，不能因 negate 变 true。（验证：运行存在命中/不命中、缺失、错误类型和动态路径矩阵；期望缺失类全部不匹配。）

- [x] C029：加载器按目标事件校验字段可用性，只有 `tool.arguments.*` 可以动态扩展；其他未知或不可能字段在启动期拒绝，运行时缺失的动态参数按 false/动作失败处理。（验证：逐事件加载静态/动态路径样本；期望错误阶段和运行结果准确。）

- [x] C030：Hook 复用权限模块的 exact/glob 基础匹配语义，但 regex/negate 只在 Hook 配置可用；既有权限 YAML 继续拒绝新增 matcher，旧决策结果逐项不变。（验证：对同一 exact/glob fixture 比较新旧权限入口，并向权限 YAML 注入 regex/negate；期望兼容结果一致且扩展字段被拒绝。）

- [x] C031：Prompt/SubAgent 模板只接受完整 `{{point.path}}` 标量占位符，不支持函数、循环、条件、嵌套表达式或部分 token；字段缺失或得到对象/数组时整次渲染失败，不注入任何部分文本。（验证：运行合法标量、动态工具参数、非法 token、缺失和复合值测试；期望合法逐字替换、非法原子失败。）

## 四类动作

- [x] C032：每条规则恰有一个动作，四类动作的必选字段、默认值和未知字段均由严格 schema 统一验证；单个候选失败时不会发布部分 Engine 快照。（验证：运行四类最小/完整配置及交叉字段样本；期望快照全有或全无。）

- [x] C033：Command 以静态非空命令在绝对项目根通过 `/bin/sh -c` 执行；基础环境只继承 PATH/HOME/TMPDIR/LANG/LC_ALL/LC_CTYPE/TERM 并重设 PWD，静态 env 可覆盖；只在启动期展开 `${ENV_NAME}`，`$${...}` 保持字面，未定义变量、NUL 和事件占位符均拒绝。（验证：运行记录 cwd、argv、完整环境及展开正反例；期望目录、shell、允许/禁止变量、转义和失败阶段精确。）

- [x] C034：Command stdin 是与本次条件/HTTP/Prompt 相同的规范 EventContext JSON；命令和 env 不能插入事件字段，Hook 内部命令不会再次触发生命周期 Hook。（验证：让 helper 捕获 stdin/env 并记录 Hook 调用数；期望 JSON 相等、静态字段未渲染且无嵌套 dispatch。）

- [x] C035：Command 的启动失败、非零退出、stdin/stdout/stderr I/O 错误、取消、timeout 和输出超限都能收束；Unix 取消只对独立进程组执行 TERM→grace→KILL→join，正常路径不发 signal，且不泄漏子进程/pipe goroutine；动作只形成技术失败并允许主流程和后续规则继续。（验证：逐类注入故障和进程树并运行 race；期望信号顺序、资源归零、once 释放且不产生 deny。）

- [x] C036：Command stdout/stderr 分别受 32 KiB 限制并先限长、脱敏再进入任何结果；stderr 永不参与 decision，原始流不持久化，limit+1 不能截断后误判 allow/deny。（验证：运行双流 limit/+1、secret canary 和 stdout-only decision 测试；期望超限整体失败且所有输出面无原值。）

- [x] C037：HTTP method 默认 POST 且只接受 GET/POST/PUT/PATCH/DELETE，`send_event` 默认 true 并发送规范 JSON 与唯一 `application/json`；false 时不构造事件 body，静态 Header 可启动期展开环境变量，Host/Content-Length 和冲突 Content-Type 均在加载期拒绝。（验证：用 loopback recorder比较 method、headers、body 和默认/false 分支，并加载非法 Header；期望请求与失败阶段准确。）

- [x] C038：HTTP URL 必须静态、绝对、无 fragment/userinfo/opaque/control character，不展开环境或事件字段；目标只允许 HTTPS 或精确 localhost/loopback IP literal 的 HTTP。加载期不做 DNS，请求 localhost 时每个运行期解析候选都必须是 loopback，环境 proxy 不能截获请求。（验证：用 URL 负例及注入 resolver/dialer 覆盖公网明文、合法 IP、混合 DNS、DNS rebinding 和 proxy；期望仅合法目标建立连接。）

- [x] C039：HTTP 最多跟随 3 次重定向，每跳重新验证协议和 loopback，并拒绝跨 Origin 与 HTTPS 降级；301/302/303/307/308 的 same-origin 跳转都保留原 method 并从有界副本重建原 body。（验证：运行重定向矩阵；期望第 3 跳成功且请求不退化，第 4 跳和所有越界跳转失败。）

- [x] C040：HTTP 非 2xx、网络错误、timeout、非法重定向、响应 Header 超过 64 KiB、解压后 body 超过 64 KiB 或 request body 超过 1 MiB 均整体失败并 fail-open；认证 Header、query、body 和 response canary 不泄漏。（验证：用本地 server 跑 limit/+1 和故障矩阵；期望后续规则/Agent 继续且诊断无原值。）

- [x] C041：Prompt 内容按事件模板原子渲染，多条按 sequence 与有效规则顺序追加且保留独立来源边界；固定注入位置在不可变安全指令之后、普通 instructions/memory、Skill catalog/SOP 与 runtime reminder 之前，不能替换安全块。（验证：捕获 ordered system blocks；期望精确顺序、边界和安全块内容不变。）

- [x] C042：Hook Prompt 只进入 main Agent Loop 与 isolated Skill Agent Loop，不进入 compact summary、Memory 更新或其他内部 Provider 请求，也不自动写 Conversation。（验证：分别捕获主请求、独立请求、摘要请求和 Memory 请求；期望仅前两类出现 Prompt。）

- [x] C043：Prompt scope 默认 turn；next 在存在 execution 时绑定 execution，否则绑定 session。session_start 只允许 next/session，turn_start、message_*、tool_*、compact_* 允许 next/turn/session，turn_end 只允许 session，system_* 与 session_end 不允许 Prompt；矩阵外组合、Prompt timeout/async、目标事件不可能提供的占位符均在启动期失败。Turn 外 `/compact` 的 turn scope 只诊断并 fail-open。（验证：加载完整兼容矩阵并执行 session/system/`/compact` 等边界；期望 binding、早期失败和运行期缺 owner 结果准确。）

- [x] C044：next、turn、session 的可见性符合定义：next 只进入下一次合格发送，turn/session 在同一作用域每次上下文重建都稳定存在；多个请求竞争 session-next 时只有一个 lease owner。（验证：运行多 iteration、main/isolated 并发和 context rebuild 测试；期望 next 恰好一次、持久 scope 每次出现且无重复租借。）

- [x] C045：Prompt 在同一锁内执行片段、owner 和当前请求可见总量检查，通过才原子追加；pre-send 失败释放 next，已写出则消费，新增条目进入下一 generation，取消 waiter 不泄漏 gate。（验证：运行 limit/+1、发送前/后取消和并发 barrier；期望不部分提交、不重复注入、无死锁或 goroutine 泄漏。）

- [x] C046：合法 SubAgent 配置可启动；触发时只记录 `hook_subagent_not_implemented` 并返回成功 no-op，可按规则结算 once，但不创建 Agent、decision 或任何嵌套 lifecycle。（验证：用 Agent/lifecycle/decision recorder 触发同步与异步占位；期望三类计数为 0、诊断恰好出现且 once 行为准确。）

- [x] C047：Command、HTTP 和 SubAgent 内部活动均不会递归触发 Hook；HTTP `send_event=false` 不因超大事件 payload 失败，其他 payload action 对同一超大事件统一失败。（验证：用递归 recorder 和 1 MiB+1 EventContext 对照执行；期望无嵌套事件且静态 HTTP 独立成功。）

## Dispatcher、once、异步与关闭

- [x] C048：同步规则严格按用户文件声明顺序再到项目文件声明顺序逐条完成，不能按动作类型重排；异步只保证按该顺序入队，不承诺完成顺序。（验证：混合四 action 并人为扰动执行耗时；期望同步完成序和异步 admission 序稳定。）

- [x] C049：条件、模板、Command、HTTP、decision、队列和意外 panic 的技术失败均被单规则隔离并 fail-open，后续规则、消息写入、普通工具链、turn_end 与应用关闭仍运行。（验证：在规则序列每个阶段逐一注入故障/panic；期望除合法 deny 外主流程终态与无故障基线一致。）

- [x] C050：同步 `once: true` 只在 event/condition 已匹配后、动作前原子执行 idle→pending；no-match 不消费 once，并发看到 pending/done 立即跳过。成功/合法 allow或deny/SubAgent no-op 提交 done，失败、timeout、cancel 或 panic 释放回 idle，进程重启后重置。（验证：用 no-match、barrier 并发及重建 Engine 场景；期望消费时点、同进程最多一次成功、失败可重试和新进程重置准确。）

- [x] C051：异步 once 在入队前使用同一原子保留；queue reject、排队过期、运行失败、取消或 panic 释放，成功/no-op 提交，并发调用不会重复入队或重复成功副作用。（验证：运行异步竞争和各 outcome 表；期望 reservation 最终状态与副作用计数准确。）

- [x] C052：生产异步执行器最多 4 个任务运行、64 个任务等待；没有异步规则时不启动 worker，第 65 个等待任务立即拒绝、释放 once 并写 queue-full 诊断而不阻塞 Agent。（验证：用 barrier 固定 4 running/64 queued，再提交一项；期望容量、延迟和 worker 计数精确。）

- [x] C053：异步任务在入队前冻结一次有界不可变 payload，不保留 Conversation/Turn 可变引用；timeout 从成功入队时开始并包含排队时间。过期 job 不执行 action，Turn/Session 结束不取消已接纳 job，只有规则 deadline 或 Engine root shutdown 能取消。（验证：入队后变异原对象，并用可控 clock/queue 驱动排队过期和作用域结束；期望 runner 输入、执行与结算计数准确。）

- [x] C054：Command 默认 30s、HTTP 默认 10s，显式正数和 10 分钟上限对同步/异步动作统一生效；timeout 只形成安全诊断并 fail-open，不会留下进程、请求、Prompt 部分状态或错误 once 状态。（验证：运行默认/边界/超限及各 action timeout 测试；期望启动或运行阶段和清理结果准确。）

- [x] C055：async action 的 completion 不能改变已经返回的工具决定或事件结果；后台失败只写诊断，不向普通 TUI、Conversation 或后续 Provider 请求注入结果。（验证：让异步动作在 dispatch 返回后分别成功/失败；期望调用方结果不变且只有 Diagnostics 有记录。）

- [x] C056：退出先同步完成 `system_stop` 分发并允许其异步动作 admission；只有该同步 dispatch 返回后才开始异步关闭窗口：停止新 admission、正常 drain 最多 2s，再取消剩余任务并最多 post-cancel join 1s。重复/并发 Close 无 send-on-closed、泄漏、重复关闭或死锁。（验证：用 barrier 覆盖慢同步 stop、drain 成功、deadline cancel、enqueue-vs-close 和重复 Close，并运行 race；期望异步阶段最多 3s，且不错误限制此前各自受 rule timeout 的同步 stop。）

## 工具决策与不可绕过安全链

- [x] C057：工具前置顺序固定为 Registry/Skill 可见性与严格 JSON 结构预检 → Plan policy → 参数规范化 → 硬安全 → Hook `tool_before` → 普通权限/用户确认 → handler → 安全化结果 → `tool_after`；每阶段复用同一份已验证/规范化调用。（验证：使用逐阶段 recorder 跑 allow、malformed JSON 和 Normalize failure；期望顺序、停止边界、参数类型和 fingerprint 完全一致。）

- [x] C058：未知工具、Skill 隐藏、Plan deny 和硬安全 deny 在 Hook 前停止；Plan deny 不触发参数规范化，Hook allow 不能伪造授权 Grant，`load_skill` 只保留既有普通权限豁免而不绕过前置 gate。（验证：运行前置拒绝矩阵和 `load_skill` 专项测试；期望 recorder 在正确边界停止。）

- [x] C059：只有同步 `tool_before` Command/HTTP 的合法 `decision: true` 能影响工具决定；合法 allow 继续剩余 before 规则以及普通权限和用户确认。（验证：分别由真实 Command/HTTP 返回 allow，并让后续 Hook 与 ordinary ask/deny 生效；期望全链继续。）

- [x] C060：首个合法 deny 立即停止剩余 before，不弹普通确认、不执行 handler、不触发 after；它产生 status=denied、code=`hook_denied`、recoverable=true 和安全非空 reason，并沿现有 tool_result 历史反馈下一次模型请求。（验证：捕获 UI、Conversation、handler 和下一 Provider 请求；期望只有规范拒绝结果回流。）

- [x] C061：Command decision 只读取完整 stdout，HTTP decision 只读取成功响应 body；前后 JSON 空白可接受，但额外输出、重复/未知 key、无效 UTF-8、未知 decision、净化后为空或超长的 deny reason、stderr 伪 decision 均无效；合法 reason 中的 ANSI、NUL 和非法终端控制字符会先移除再脱敏。（验证：运行精确协议正反矩阵；期望只有规范 allow/deny 两种形状生效，净化不会把超长输入变成合法拒绝。）

- [x] C062：非零退出、非 2xx、timeout、非法 decision 或 decision 超限都只记录诊断并继续普通工具安全链，绝不把截断内容解释为 deny。（验证：每类故障后配置 ordinary permission ask/deny；期望普通决策仍可观察且无 Hook 拒绝结果。）

- [x] C063：`tool_after` 只在实际 handler 返回并完成既有限长和 RuntimeRedactor 后同步触发，duration 只覆盖 handler；success/error/timeout 映射准确，随后才写 UI/Conversation。（验证：用可控 handler 时钟和 secret output 记录顺序；期望时间边界、状态和模型可见 payload 准确。）

- [x] C064：内置、MCP、`load_skill` 的 success/error/timeout 正向矩阵与全部 pre-handler 负向矩阵共同证明公共 gate 无旁路；`load_skill` 的业务参数错误和 Skill 语义失败属于已进入 handler 的 after 路径。（验证：运行公共工具 gate 综合测试；期望每行 before/after/handler 计数精确。）

- [x] C065：Hook allow、failure 和 no-match 必须继续普通权限/用户确认；合法 deny 只能提前拒绝，绝不能转化为执行授权或 Grant。所有 Hook 路径都不能扩大 Skill 工具白名单、绕过 Registry/Plan/硬安全或写入 session/local/project/user 权限。（验证：执行前后比较确认 recorder、所有权限层、Grant 和 Skill profile，并重载持久状态；期望 only-deny 短路和其余继续路径准确，持久状态逐字不变。）

## Prompt 发送握手、压缩、独立执行与 App 生命周期

- [x] C066：有 Hook Prompt 时 Provider 使用 ordered system blocks，精确顺序为 fixed safety → Hook → optional instructions/memory → catalog → active SOP → runtime reminder；无 Hook 时逐字走 legacy system path。（验证：捕获两类 Provider request 并与 golden 比较；期望有 Hook 精确分块、无 Hook wire 完全不变。）

- [x] C067：OpenAI/Anthropic wire 只发送 role/content/cache 所需字段，不发送本地 block Name、Hook source、规则身份或用户/项目绝对路径；Anthropic 只在最后一个 fixed block 设系统 cache boundary，Hook path 不缓存 tools。（验证：在 Name/source/path 放 canary 后序列化请求；期望 wire 零命中且 cache 标记位置唯一。）

- [x] C068：每个 Provider transport attempt 恰好一次终态：真正写出后 commit next Prompt，明确未写出且 callback 静止后 release；请求交给 Provider 后 caller 不得自行 Release，异步 Provider 必须先 Finish 再发送 terminal event/关闭 channel。取消与写出交错时无迟到 MarkSent、重复终态或 gate 泄漏。（验证：用 httptrace/transport barrier 覆盖 pre-send、post-write、同步和异步 Provider；期望 lease 与 terminal 顺序唯一并通过 race。）

- [x] C069：Prompt 在线性化发送边界获取；同一 Turn 的多次 Provider iteration/context rebuild 中 turn/session 每次出现，next 只出现一次，Acquire 后新增 Prompt 进入下一 generation，summary/Memory 从不取得 lease。（验证：运行多 iteration 与并发 main/isolated 发送；期望 generation、可见次数和内部请求计数准确。）

- [x] C070：上下文压缩 preflight 不修改 Conversation、metadata 或 blob；真正 attempt 才触发 Hook，临时 isolated prepare 不持久化产物，observer 自身失败不改变既有 compact 结果。（验证：比较调用前后深拷贝、Store 文件和返回值；期望无孤儿产物且 Hook fail-open。）

- [x] C071：turn_end 清除 turn 与未消费 execution-bound next，session_end/切换清除 session 与 session-bound next；owner 结束后的迟到 lease/异步操作只能 no-op，`/clear` 不错误清除仍有效 Hook Prompt。（验证：在各边界前后重建请求并触发迟到 Commit/Release；期望无过期残留、复活或误清理。）

- [x] C072：main 与 isolated 共享规则快照、Diagnostics、async pool、进程 once 和 session Prompt；execution-next/turn Prompt、turn status、Skill profile 与模型按 execution 隔离，父子并行不串扰。（验证：用 barrier 并发运行 main/isolated 并比较状态；期望共享项唯一、隔离项互不可见。）

- [x] C073：isolated 临时 Conversation 中的逻辑 user/final assistant 是唯一 Message Hook 来源，主 Conversation 的 raw command/最终摘要 bridge copy 不重复触发；摘要回流仍保持既有主历史语义。（验证：运行独立 Skill 后比较临时 recorder 与持久消息；期望每个逻辑消息一组事件且无重复生命周期。）

- [x] C074：用户取消只发 cancel 并等待真实 run 完成 EndTurn；stream channel 无 terminal event 时有显式 closed 路径，App 只有在 terminal/closed 且 run tracker idle 后才恢复输入或执行第二次退出。（验证：运行正常 terminal、无 terminal close 和取消竞争；期望 canceled turn_end 永远先于 UI 清理/退出。）

- [x] C075：App Close 幂等地执行 cancel/WaitIdle → final save → session_end(exit) → session/Skill/mode 清理，且不关闭 Hook/MCP；main 是进程资源唯一 owner，随后用独立 context 依次 Hook Shutdown → MCP Close。（验证：运行正常、TUI error、App 未创建和重复关闭；期望每个资源只关一次且一个超时不吞掉后续预算。）

- [x] C076：system_start 前的下游构造失败不产生 system_stop，但已创建资源仍各关闭一次；TUI 退出后产生的 shutdown 诊断同时进入 Collector 和无 payload 的安全 stderr 摘要。（验证：在各 factory 边界注入失败和迟到 cancel；期望事件、closer、stderr 与 Collector 计数准确。）

## 诊断、脱敏与资源上限

- [x] C077：每类运行期失败都有稳定诊断码，至少覆盖 condition、template、command、http、invalid decision、timeout、async queue full、shutdown cancel、SubAgent placeholder、Prompt scope、limit 和 panic；单规则记录含 event、来源、规则序号、action、stage、duration 和安全摘要，跨规则 shutdown 汇总则含 event、stage、真实 duration 与取消/未完成 count，且不伪造规则身份。（验证：逐类制造一次失败并读取 Collector JSON/Text；期望字段可定位、排序稳定、汇总数量真实且摘要有界。）

- [x] C078：Hook 诊断只进入有界 Diagnostics Collector并可由 `/diagnostics` 查看，不进入普通 TUI message、Conversation、Provider request 或 Memory；关闭后额外 stderr 也不带 payload。（验证：触发诊断后分别搜索所有可见/持久输出；期望仅批准入口有安全摘要。）

- [x] C079：消息正文、工具参数/结果、Command stdout/stderr、HTTP body/response 和内部错误不得被复制进 Hook 诊断或错误；环境展开秘密、认证 Header、URL query 和 Hook 私有输出在进入 decision/用户可见面前统一脱敏，短 secret 只匹配完整 token/字段值而不破坏普通文本，模型可见 tool result 保持既有限长与脱敏。（验证：对消息/参数 canary 只检查 Collector、stderr、`/diagnostics` 和 Hook error/decision 为零命中；对 Hook 私有 secret 另检查普通 TUI、Conversation 和 Provider wire；期望私有 secret 零命中，且单字符 secret 不导致普通文本全局替换。）

- [x] C080：Command 字符串 16 KiB、env 64 项、env key 128 B、展开后 env value 8 KiB、HTTP URL 2 KiB、Header 32 项、name 128 B、展开后 value 8 KiB 均有 limit/limit+1 启动证据。（验证：运行静态 action 配置边界矩阵；期望 limit 通过、limit+1 在发布快照前失败。）

- [x] C081：EventContext JSON/request body 1 MiB、Command stdout/stderr 各 32 KiB、HTTP response body 64 KiB、response Header 64 KiB 和 redirects 3 次均有 limit/limit+1 运行证据。（验证：运行 payload/stream/response/redirect 边界矩阵；期望上限成功，超限停止读取或执行并 fail-open。）

- [x] C082：Prompt template 64 KiB、单片段 128 KiB、每 execution/session owner 活动总量及一次 lease 跨 owner 可见合计 256 KiB、SubAgent agent 64 B/input template 与完整渲染 64 KiB、deny reason 与诊断摘要 2 KiB 均有 limit/limit+1 证据。（验证：运行 UTF-8 与并发跨 owner 边界矩阵；期望配置期或运行期按定义失败且无部分状态。）

- [x] C083：EventContext 规范 JSON 超过 1 MiB 时内存条件仍能匹配，Command 和 `send_event=true` HTTP 在 transport 前失败，`send_event=false` 静态 HTTP 可执行；失败诊断不复制 payload。（验证：用同一超大事件驱动四条对照规则；期望选择与 action 结果精确。）

- [x] C084：任何会影响 deny 的 stdout/response/reason 超限都整体判为技术失败，不能先截断再决策；Prompt 片段或聚合超限也不能部分提交。（验证：把合法 deny JSON 跨越每个边界并在 Prompt 尾部放 canary；期望普通权限链继续、Prompt 状态零新增。）

- [x] C085：Diagnostics attributes 最多 8 项、key 最多 64 B、value 最多 256 B、序列化合计最多 2 KiB；任一边界超限时整体丢弃 attributes 而保留基础 Diagnostic，不截成部分 map。Add/List 两侧深复制并稳定排序，清理无效 UTF-8、ANSI 和终端控制字符；单条安全摘要也不超过 2 KiB。（验证：运行各项 limit/+1，在边界前后变异输入 map 并注入多字节/控制字符；期望 JSON/Text 安全、确定且无共享修改或部分属性。）

## 向后兼容、示例与信任说明

- [x] C086：没有 Hook 文件与空规则两种路径都选择 legacy Provider API，Provider wire、cache、Conversation JSON 和普通 turn 结果与引入 Hook 前的 golden 逐字节一致。（验证：分别跑无文件/空规则 scripted turn 并比较 golden；期望无 Hook 元数据或额外消息。）

- [x] C087：nil/no-op Hook 下，本地命令、Skill 激活/清理、built-in/MCP/`load_skill` 工具链、Plan、权限 Grant 与用户确认结果均与 legacy fixture 一致。（验证：对三种 Runtime 运行相同 fixture；期望输出、mode、profile、fingerprint 和工具结果逐项相同。）

- [x] C088：nil/no-op Hook 下，Memory/instructions、auto/manual compact、Session new/resume/save/switch/close、App/TUI 可见状态和持久产物均与 legacy fixture 一致。（验证：比较 prompt/context、summary、文件、Conversation JSON、消息和最终 Model；期望无 Hook event/Prompt/diagnostic。）

- [x] C089：无文件或空规则不启动异步 worker、不产生 system event/stderr/UI notice；既有 App/MCP 资源仍按旧顺序各关闭一次，只增加可忽略的初始化开销。（验证：记录两条启动/关闭路径的 worker、event、stderr 和 closer；期望额外计数为 0。）

- [x] C090：权限 YAML 仍只接受旧 matcher，Permission 旧入口与分阶段新入口对 deny/ask/allow、Grant、大数参数和 fingerprint 给出相同结果；Hook allow 不削弱 Executor 的 Grant 校验。（验证：运行新旧对照与伪造 Grant 测试；期望逐行等价且伪造执行被拒绝。）

- [x] C091：版本控制中的示例可由真实 Loader 解析并覆盖四 action、合法 all/any 之一、once/async/timeout、decision 和三种 Prompt scope；`.gitignore` 只放行 `.xagent/hooks.yaml`，不放行同目录其他本地状态。（验证：加载示例并运行 `git check-ignore --no-index` 正反检查；期望示例有效且忽略边界精确。）

- [x] C092：README 明确两级路径与累加顺序、schema、动作/条件/执行控制、Prompt scope、修改后重启，并显著警告项目 Hook 是受信代码，可在普通确认前执行 Shell/HTTP、外传事件、注入 system Prompt，技术失败时 deny 会 fail-open；还披露大量同步规则及单规则 10 分钟 timeout 可长时间阻塞且本版无 aggregate timeout/热更新，打开不可信项目应先审查。（验证：自动检查关键主题并由人工阅读信任警告段；期望每项均可定位且措辞不把 Hook 描述为不可绕过安全边界。）

## 自动化质量门禁

- [x] C093：Hook 领域包的 Loader、Event、Condition、Template、Command、HTTP、Prompt、once、async、Engine、Diagnostics 和全部 limit/+1 测试一次性通过。（验证：运行 `go test -count=1 ./internal/hook`；期望返回 `ok`。）

- [x] C094：matcher、permission、tool、prompt、provider、orchestrator、contextmgr、sessionctx、app、tui 和 main 的定向集成/兼容测试全部通过，且只使用 fake、scripted Provider 或 loopback server。（验证：运行 `go test -count=1 ./internal/matcher ./internal/permission ./internal/tool ./internal/prompt ./internal/provider ./internal/orchestrator ./internal/contextmgr ./internal/sessionctx ./internal/app ./internal/tui ./cmd/xagent`；期望全部 `ok` 且无公网访问。）

- [x] C095：tagged TUI 驱动完整包含并通过 `TestHookE2EDriverSmoke`、`TestHookE2ELifecyclePrompt`、`TestHookE2EActionTransport`、`TestHookE2EToolDecision`、`TestHookE2EAsyncOnce`、`TestHookE2EIsolatedSkill`、`TestHookE2EFailureDiagnostics`，其中 async/once 场景另通过 race。（验证：先运行 `go test -tags=integration ./internal/app -list '^TestHookE2E'` 并核对上述七个名称，再运行 `go test -tags=integration -count=1 ./internal/app -run '^TestHookE2E'` 与 `go test -tags=integration -race -count=1 ./internal/app -run '^TestHookE2EAsyncOnce$'`；期望名称集合完整、七项全部执行通过且无 race。）

- [x] C096：Diagnostics/Redactor、once、Prompt lease/gate、Provider observer、async admission/close、run tracker、compact 和 App lifecycle 无已检测数据竞争或低概率关闭错误。（验证：运行 `go test -race -count=1 ./internal/diagnostics ./internal/redact ./internal/hook ./internal/provider ./internal/orchestrator ./internal/contextmgr ./internal/app`、`go test -race -count=20 ./internal/hook -run 'TestOnce|TestPromptConcurrent|TestAsync.*Close|TestAsync.*Shutdown'` 和 `go test -race -count=20 ./internal/provider ./internal/orchestrator -run 'Test.*RequestAttempt|Test.*NextPrompt'`；期望全部无 race、panic、死锁或泄漏。）

- [x] C097：规则排序、once、Prompt、工具链和 Turn 生命周期不依赖测试顺序或偶然调度。（验证：运行 `go test -shuffle=on -count=20 ./internal/hook ./internal/orchestrator`；期望 20 轮全部通过，无偶发失败。）

- [x] C098：全部 Go 文件符合 gofmt。（验证：运行 `test -z "$(gofmt -l $(rg --files -g '*.go'))"`；期望无输出且退出码 0。）

- [x] C099：补丁没有尾随空白、冲突标记或空白错误。（验证：运行 `git diff --check`；期望退出码 0。）

- [x] C100：全项目静态检查通过。（验证：运行 `go vet ./...`；期望退出码 0。）

- [x] C101：全项目自动化测试通过，现有命令、Skill、Memory、MCP、权限、会话和 Agent Loop 无回归。（验证：运行 `go test -count=1 ./...`；期望所有包返回 `ok`。）

- [ ] C102：当前工作区源码成功构建为新二进制，非 Unix Command shim 可交叉编译，且最终差异只包含已批准的 Hook 系统代码、测试、README、示例/忽略规则和四份规格文档。（验证：运行 `go build -o /private/tmp/xagent-hook-system ./cmd/xagent`、`go version -m /private/tmp/xagent-hook-system`、`GOOS=windows GOARCH=amd64 go test -c ./internal/hook -o /private/tmp/xagent-hook.test.exe`、`git status --short` 与 `git diff --stat`；期望本机构建与 Windows 编译成功、模块为当前项目且范围与 task.md 对齐。）

## 自动化验收证据（2026-07-23）

- C001–C092：Hook 领域单元测试、跨包兼容测试和 tagged TUI 驱动覆盖了 Loader/Schema、事件身份与顺序、条件与模板、四类 Action、工具拦截、Prompt scope、once/async、关闭协议、诊断与敏感信息处理、硬限制和无 Hook 兼容路径；最终规格与实现审计发现的问题均已修复，未保留已知阻断项或中等级问题。
- C093：`go test -count=1 ./internal/hook` 通过。
- C094：`go test -count=1 ./internal/matcher ./internal/permission ./internal/tool ./internal/prompt ./internal/provider ./internal/orchestrator ./internal/contextmgr ./internal/sessionctx ./internal/app ./internal/tui ./cmd/xagent` 全部通过。
- C095：`go test -tags=integration ./internal/app -list '^TestHookE2E'` 列出规定的七个场景；`go test -tags=integration -count=1 ./internal/app -run '^TestHookE2E'` 与 `go test -tags=integration -race -count=1 ./internal/app -run '^TestHookE2EAsyncOnce$'` 均通过。
- C096：清单规定的三组 race 命令全部通过；另以 `-race -count=10` 重复覆盖决策 reason 脱敏、主/独立 turn 取消、Provider 错误、stream 取消竞态、shutdown admission、Command 进程组和超大十进制输入，均通过。
- C097：`go test -shuffle=on -count=20 ./internal/hook ./internal/orchestrator` 通过。
- C098–C100：全量 Go 文件 `gofmt -l` 无输出，`git diff --check` 与 `go vet ./...` 均以退出码 0 完成。
- C101：最终 `go test -count=1 ./...` 全部通过。此前一次全仓运行曾出现 MCP stdio 启动超时；定向重跑 `go test -count=1 ./internal/mcpclient` 及后续两次全仓运行均通过，判定为瞬时环境抖动。
- C102：本机二进制构建、`go version -m` 模块检查和 Windows/amd64 Hook 测试二进制交叉编译均通过。该项仍保持未勾选，因为当前 `git status` 包含需保留的用户文件、工作树元数据及此前 Command/Skill 工作，无法证明“最终差异只包含本 Hook 范围”；未删除或改写这些既有内容。
- E01–E10：依用户要求保留为真实 TUI 人工端到端待验收，不以自动化测试替代勾选。

## 真实 TUI 端到端场景

> E01–E10 使用当前源码构建的 XAgent、PTY/真实 Bubble Tea 输入循环、本地 scripted OpenAI-compatible Provider、隔离临时 HOME/项目和 loopback HTTP；禁止真实 Provider、公网和用户真实配置。每项需保留命令、PTY capture、事件 JSONL/请求 JSONL 及退出码摘要。

### E01：两级配置、稳定顺序与早期失败

- [ ] 同时放置用户级和项目级 Hook，让每层两条 Command 把 EventContext 写入隔离 recorder；启动真实 TUI 后触发一次事件，观察四条记录严格按用户→项目及声明顺序。随后加入未知字段重启，程序应在 alternate screen 和 Provider 请求之前非零退出，错误含脱敏的文件/行列/规则/字段定位；删除任一文件仍可启动。（验证：保存两次进程退出码、PTY capture、Provider 请求计数和 recorder JSONL；期望成功/失败/缺失文件三条路径与 C001–C005 一致。）

### E02：主会话生命周期、本地命令与关闭顺序

- [ ] 在真实 TUI 完成一个含两次 Provider iteration 的普通 turn，依次执行 `/plan`、`/do`、`/compact` 并切换一次 Conversation；随后启动第二个被 scripted Provider 阻塞的 turn 并触发退出。事件日志应有一组 completed 和一组 canceled turn，完整 user/assistant message 成对，普通本地命令无虚假 turn/message，compact 与 session 切换产生真实事件；App.Close 等 canceled turn_end 后才 save→session_end，返回后才 Hook.Shutdown/system_stop→MCP.Close。（验证：对照 PTY、事件 JSONL 和 closer recorder 的 sequence；期望每个身份、状态、reason、计数和 owner 顺序准确，且不伪造第三组 turn。）

### E03：Prompt 注入、三种 scope 与发送边界

- [ ] 配置 next/turn/session Prompt canary，让 scripted Provider 在一个 turn 内请求两次并捕获 ordered system blocks；next 只出现一次，turn/session 两次都出现且位于固定安全块后、Skill/运行时块前。制造一次 Provider pre-send 失败后重试，next 应释放再出现；完成 turn、切换 session 后相应 Prompt 消失，compact summary 和 Memory 请求始终不含 canary。（验证：保存每次请求 JSONL 与事件日志；期望 scope、顺序、清理和 request-attempt 语义全部可见。）

### E04：Command 与 HTTP 传输且无递归

- [ ] 用含 string/number/bool/object/array 工具参数的同一事件同时触发条件、Command helper 和 loopback HTTP endpoint；两种 action 收到逐字相同的规范 EventContext且类型不丢失，Command cwd/env、HTTP method/header/body 符合配置，规则顺序稳定，输出/响应不进入普通消息或 Conversation，也不产生嵌套 Hook。（验证：比较条件命中、stdin、HTTP capture、PTY、Conversation 和事件 JSONL；期望快照/类型相等且递归计数为 0。）

### E05：工具 allow、deny 与普通权限

- [ ] 让本地 Provider 依次请求一个需确认的内置工具、一个需确认的 MCP 工具和 `load_skill`：Hook allow 时前两者仍显示既有用户确认，`load_skill` 保留系统工具的普通权限豁免但仍经过 Registry/Plan/hard/Hook；三者实际执行后都有 after。Hook deny 时均不确认、不执行、不激活 Skill、无 after，下一请求收到 `hook_denied`、recoverable=true 和安全 reason。（验证：保存确认画面、工具/Skill recorder、事件日志及下一次 Provider request；期望 allow/deny 与系统工具豁免边界准确且权限文件 hash 不变。）

### E06：timeout、技术失败与诊断隔离

- [ ] 让 Command 超时、HTTP 返回非 2xx，并在命令流、Header、query、body、response、工具参数和错误中放不同 canary；Agent turn 应 fail-open 完成，普通权限链仍运行，执行 `/diagnostics` 才看到稳定安全码与来源定位。消息/工具参数只要求不被复制到诊断，Hook 私有 Header/query/输出/响应 secret 还不得进入普通 TUI、Conversation 或 Provider。（验证：按来源域分别扫描 PTY、Collector、Conversation 和请求 JSONL；期望主流程成功、诊断域不含任何原 payload，私有 secret 在全部非 action 接收面零命中。）

### E07：异步、once 与有界退出

- [ ] 用 barrier helper 并发触发 `async: true, once: true` 规则，重复事件只产生一次成功副作用；退出时 `system_stop` 的异步动作先被接纳，随后按 App.Close→Hook.Shutdown→MCP.Close 受管 drain/cancel，异步阶段在 2s+1s 窗口内收束且无残留 helper/worker。（验证：记录副作用计数、admission/finish/closer 时间线、进程耗时与残留进程检查；期望 once=1、owner 顺序准确、关闭有界且无泄漏。）

### E08：独立 Skill 的共享与隔离

- [ ] 在主 turn 中触发一个 isolated Skill，并让父子都使用工具、compact 和 Prompt；child 事件使用 isolated kind 且无额外 session，进程 once/session Prompt 与父级共享，execution-next/turn Prompt 和状态互不串扰，主历史的 command/summary bridge 不重复产生 Message Hook。（验证：比较父子 execution/turn ID、Prompt 请求、事件 JSONL 和重载后的 Conversation；期望共享/隔离矩阵和消息计数准确。）

### E09：无 Hook 的可见与线协议兼容

- [ ] 分别以两个 Hook 文件都缺失和存在空规则启动真实 TUI，运行同一普通对话、命令、Skill、tool、compact、session 保存/恢复并退出；两次 Provider wire、Conversation JSON、权限结果、TUI capture、资源关闭记录应与 legacy golden 相同，且无 worker、Hook notice、system event 或 stderr。（验证：对所有产物做规范化 diff；期望零行为差异。）

### E10：配置/资源边界修复后可恢复启动

- [ ] 在隔离项目依次放入未知 schema、非法 Prompt scope/async 组合和一个静态资源 limit+1 配置，确认每次都在 TUI 前安全失败且无副作用；修正为恰好 limit 的合法示例后，真实 TUI 成功启动、`/diagnostics` 可用并正常退出。（验证：保存四次退出码、stderr/PTY、factory或本地服务计数和最终启动画面；期望三次早失败、一次成功且错误不泄漏配置 secret。）

## 验收标准覆盖

| Spec 验收标准 | Checklist 条目 |
| --- | --- |
| AC1 | C001–C006、C009、E01、E10 |
| AC2 | C010–C021、C074–C076、E02、E08 |
| AC3 | C014–C016、C073、E02、E08 |
| AC4 | C017–C018、C057–C064、E05 |
| AC5 | C022–C024、C034、C037、C041、E04 |
| AC6 | C025–C031、C090 |
| AC7 | C033–C036、C047、C079–C084、E04、E06 |
| AC8 | C037–C040、C080–C083、E04、E06 |
| AC9 | C031、C041–C045、C066–C071、C082–C084、E03 |
| AC10 | C046 |
| AC11 | C057–C065、E05 |
| AC12 | C035–C040、C061–C062、C084、E06 |
| AC13 | C005、C048–C049、C060、E01 |
| AC14 | C050–C051、E07 |
| AC15 | C008、C052–C055、E07、E10 |
| AC16 | C056、C075–C076、E07 |
| AC17 | C035、C040、C049、C054–C055、C062、C070 |
| AC18 | C003、C077–C085、E06、E10 |
| AC19 | C021、C072–C073、E08 |
| AC20 | C012、C043–C045、C069–C073、E03、E08 |
| AC21 | C057–C065、C090、E05 |
| AC22 | C030、C066、C086–C090、E09 |
| AC23 | C009、C024、C035–C036、C044–C056、C068–C085、C096–C097、E07–E08 |
| AC24 | C091–C092 |
| AC25 | C093–C102、E01–E10 |

全部 AC1–AC25 均至少映射到一个可运行或可观察条目。自动化条目 C001–C101 已取得证据并通过；C102 因工作区范围无法隔离而保持待验收，E01–E10 按约定保留为真实 TUI 人工端到端待验收。
