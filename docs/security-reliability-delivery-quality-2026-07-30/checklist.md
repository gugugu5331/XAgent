# XAgent 安全、可靠性与交付质量提升 Checklist（2026-07-30）

> 状态：已批准（2026-07-31）
>
> 前置文档：`spec.md`、`plan.md`（含 C1–C17）和 `task.md` 均已批准
>
> 本文只定义可观测验收。只有实际运行结果可以勾选，计划文本、测试代码、CI 配置和预期输出均不能充当通过证据。

## 验收记录规则

- [ ] V1 每个已勾选条目都记录实际 40 位小写 commit OID、可复现命令或操作、实际结果、对应 safe evidence digest 和不可变 CI job identity；任一字段缺失时保持未勾选。（验证：运行 docs/repoaudit profile，期望拒绝无 revision、无结果、短 SHA、旧证据或仅引用计划的勾选项。）
- [ ] V2 所有命令均在 fresh checkout 上运行，checker 接收的 revision 与实际 `HEAD^{commit}` 逐字一致；不同 revision、worktree 未提交状态、tag、branch、短 SHA 或多次结果拼接不得混用。（验证：分别提交正确 OID 和错误/符号 revision，期望前者进入检查、后者在执行门禁前失败。）
- [ ] V3 测试输出、provider evidence、artifact 和诊断只保留批准的 typed safe fields，不含 raw stdout/stderr、凭据、用户数据、私有路径或 fixture 正文。（验证：运行 safe artifact/codec 负例和 canary 扫描，期望额外文件、未知字段、raw 内容及 canary 原文全部被拒绝。）
- [ ] V4 checklist 中 AC1–AC38 的完成结果只绑定 R0；E 只允许更新 README、docs index 与本 checklist 的唯一 evidence marker，仓库外 attestation 绑定 E，避免 OID 自引用。（验证：运行 docs、marker delta 与 attestation 审计，期望 R0/E 关系和三文件精确 delta 通过，任何其他文件变化失败。）

## 功能验收：仓库与权限

- [ ] AC1 / F1：已确认的 35 个会话、记忆、worktree gitlink 和大型产物全部退出 Git index，但对应本地对象、类型、mode、内容摘要和 `.gitmodules` absent sentinel 保持不变；再次运行项目后它们仍不会成为待提交内容。（验证：在同一 preservation manifest 上依次核对 freeze、remove-index、verify-immediate、after_r0 与 after_e，期望 count=35、集合 digest一致、commit repoaudit通过且本地对象未被删除或改写。）
- [ ] AC2 / F2：canary secret、无映射 gitlink、私有目录文件和 1 MiB+1 的超限二进制分别使提交检查失败，正常源码、精确 fixture 豁免、恰好 1 MiB 边界按策略处理，所有报告不显示 secret 原文。（验证：对 synthetic index 运行 repo-check 正反例，期望四类违规均为 finding、工具错误与 finding 分类不同、输出无 canary。）
- [ ] AC3 / F3：单行命令获授权后，仅增加换行、空格、引号或控制符并产生额外语义的命令不能复用 once/session/permanent 授权。（验证：运行授权碰撞 E2E，期望变体重新确认或拒绝，原始命令身份与变体身份不同。）
- [ ] AC4 / F4：直接路径、变量展开、字符串拼接、解释器动态构造及普通 Write/Edit/Bash 均无法修改受保护权限文件，任何规则或确认也不能放宽此硬约束。（验证：在 macOS、Linux、Windows amd64 原生 protected-slot 场景运行绕过矩阵，期望目标未被修改且摘要一致；保护能力不可用时必须在目标启动前 fail closed。）
- [ ] AC5 / F5：任一权限配置层损坏时，Bash、Write、Edit 和危险 MCP 调用均被拒绝；批准的低风险只读操作按保守策略继续，界面显示安全诊断。（验证：逐层注入未知版本、错误类型、损坏内容和 I/O 错误，期望危险操作无副作用、只读行为符合策略且诊断无原文泄露。）
- [ ] AC6 / F6：含凭据或宽泛目标的授权不能形成永久规则；安全规则只保存最小范围，确认界面完整展示目标、风险、授权范围、规则落点和撤销方式。（验证：运行永久授权正反例并观察确认面板与保存文件，期望敏感/过宽请求被拒绝，合法记录不含凭据且可按说明撤销。）

## 功能验收：凭据、网络、工具与资源

- [ ] AC7 / F7：同一 canary 进入 LLM key、MCP env/header、Hook、Provider/MCP 返回值与错误后，TUI、模型输入、会话 JSONL、诊断、日志、memory、safe CI artifact 和测试失败输出均无原文。（验证：运行全渠道 canary E2E及落盘内容扫描，期望所有渠道只出现安全替代文本或摘要，私有 raw artifact以外无原文。）
- [ ] AC8 / F8：非本机明文 HTTP Provider/MCP 在发送前被拒绝；HTTPS 向 HTTP、外部域名、内网或解析后禁止地址重定向均被拒绝，认证头不跨源传播。（验证：使用受控 DNS、redirect 和目标服务 fixture，期望禁止目标未收到请求或认证头，允许目标通过且每跳及拨号地址均复验。）
- [ ] AC9 / F9：远超限制的 stdout、stderr、文件、目录、长行、JSON 与 SSE 输入在采集阶段受累计预算约束，返回明确超限状态，峰值缓冲不随完整输入线性增长。（验证：运行 cap、cap+1、未知长度流和大规模输入测试，核对 Counter、原始字节数、truncated状态及内存上界。）
- [ ] AC10 / F10：超限工具输出生成工作区外、仅当前用户可读的私有 artifact；界面显示 ID、字节数和截断提示，模型、JSONL、memory 与诊断不含原文，容量或保留期到达后可清理。（验证：运行大输出 Artifact E2E，检查权限、位置、一次性终结、用户读取入口和 retention/capacity 清理结果。）
- [ ] AC11 / F11：包含后台子进程与孙进程的 Bash 或 Hook 在取消、超时或退出后 2 秒内终止完整进程树，不再产生副作用并完成有限清理。（验证：三平台原生运行进程树取消场景，观察所有后代退出、管道关闭和无迟到文件/事件。）
- [ ] AC12 / F12：Read、Grep、Glob 可及时取消；大文件、巨型目录、超长行和底层读取错误返回有界、可理解的部分结果或错误，不遗留继续遍历/分配的任务。（验证：运行取消、预算、I/O 故障和链接竞争矩阵，期望累计计数正确、结果有界且 race/泄漏检查通过。）
- [ ] AC13 / F13：重复、菱形和深层 include 不会指数展开；累计文件/字节/展开/深度超限产生诊断；并发符号链接或 reparse 切换不能越出允许根。（验证：运行 include 图、cap/cap+1 与三平台链接竞态测试，期望稳定身份、去重、非破坏跳过和 fail-closed结果正确。）

## 功能验收：Provider 与 MCP

- [ ] AC14 / F14：未知长度分块 SSE、JSON、stdio framing、工具发现和协议错误均受累计预算约束；超限中止且只保留有限安全摘要，内存不持续增长。（验证：运行 MCP HTTP/stdio恶意流、连续错误和独立JSON响应预算测试，期望明确可恢复错误、连接按契约关闭且诊断有界。）
- [ ] AC15 / F15：关闭 MCP Manager 后 receive loop、HTTP body、远程 session 和 stdio 资源全部退出；重复关闭成功，关闭后新调用被拒绝，并发状态查询/调用/关闭无 race或死锁。（验证：对初始化成功、部分失败、并发Close和远端清理次数运行 race `count=20`，期望唯一清理、lease排空和幂等终态。）
- [ ] AC16 / F16：stdio MCP 初始化失败会回收进程；大量 stderr 被持续排空但有限保留；突发响应后退出不 panic；取消阻塞写不会累积 goroutine或发生通道关闭竞争。（验证：三平台原生运行 init-fail、stderr burst、response burst、blocked writer 与 repeated close，期望进程/pipe/writer/supervisor全部收敛。）
- [ ] AC17 / F17：Provider 在正常完成、解析错误、网络错误、取消和消费者提前退出五种路径均关闭底层流，扫描错误不会被误报为正常 EOF。（验证：OpenAI/Anthropic及Orchestrator五出口 race矩阵重复20次，期望每条路径唯一Close、错误分类正确且无阻塞生产者。）
- [ ] AC18 / F18：响应按“结束原因→usage→流结束”到达时仍产生准确 usage，状态栏、上下文管理和持久化收到同一数值且只提交一次。（验证：运行 scripted Provider usage 顺序场景，比较Provider、Orchestrator、App与Conversation记录的usage。）

## 功能验收：会话与编排

- [ ] AC19 / F19：超过64 KiB但未超系统上限的合法记录可保存并在重启后加载；真正超限、中间坏行或其他损坏会话仍在列表中并带安全诊断。（验证：运行v2 codec、torn-tail、中间损坏、列表部分结果和重启测试，期望合法记录语义一致、损坏项不静默消失。）
- [ ] AC20 / F20：只修改工具外置状态、摘要或元数据而不改变消息数量，保存并重启后修改仍完整存在。（验证：加载既有会话，分别修改三类状态并执行保存/重启/重载比较。）
- [ ] AC21 / F21：会话目录无权限或扫描错误在界面与诊断中可见而非显示空历史；含可恢复坏行的会话仍可打开、继续对话并再次保存。（验证：注入列表I/O错误和可恢复坏行，观察partial result、错误展示及后续成功保存。）
- [ ] AC22 / F22：保留期限、扫描文件上限、扫描字节上限和时间跨度提醒四项配置分别真实改变生产行为。（验证：固定Clock下逐项改变配置并比较清理、列表截断、扫描摘要和提醒事件。）
- [ ] AC23 / F23：同批两个阻塞只读工具确实重叠执行但不超过并发上限，最终结果严格按原调用序号回灌。（验证：用Gate/Trace阻塞两个工具，观察并发区间、峰值和有序提交。）
- [ ] AC24 / F24：在ToolPending与真正启动之间取消时不产生伪造或空工具结果，历史能区分未启动、已取消和已执行。（验证：在资格、排队和启动barrier逐点取消，比较结果状态、事件和持久化记录。）

## 功能验收：界面、命令与配置

- [ ] AC25 / F25：不退出应用即可完成“新建会话→返回列表→恢复旧会话→切换到新会话”，每次切换前旧会话完成WaitIdle与保存；冷启动无active时不伪造保存或SessionEnd。（验证：从真实parser/key update层驱动命令与按键路径并在重启后检查两会话内容。）
- [ ] AC26 / F26：完成一轮后开始下一轮并切换会话，旧耗时、token、缓存、停止原因、错误、确认和临时状态均不会串入新请求或新会话，迟到事件也不能回填。（验证：在两个reset边界注入旧generation事件，比较三层App状态与ViewModel。）
- [ ] AC27 / F27：80×24、宽屏和运行中动态缩放时，长输入、长消息、确认面板、会话列表和历史滚动均可达、可回看且不panic或覆盖关键信息。（验证：运行三种尺寸及连续resize的TUI行为测试并观察布局。）
- [ ] AC28 / F28：显式关闭计时后不显示耗时；所有公开配置键可生效；false不被默认覆盖；负数、0禁值、cap+1、未知/重复/null字段及冲突组合在启动阶段失败；四层优先级结果确定。（验证：运行完整Config presence/merge/Resolve矩阵和config.example双向schema检查。）
- [ ] AC29 / F29：帮助中可发现全部正式命令、别名、快捷键、权限模式、状态和诊断入口；隐藏launcher及兼容命令可见性与README、规格、command metadata一致。（验证：比较生成帮助、README与metadata，期望公开集合完全一致且无隐藏接口泄露。）
- [ ] AC30 / F30：CLI帮助以成功状态输出有效用法且不要求完整运行时初始化；版本查询以成功状态输出非空、可追溯版本标识。（验证：在缺少运行配置环境中分别执行help/version，核对exit 0、stdout和版本来源。）

## 跨平台与交付质量

- [ ] AC31 / F31：macOS、Linux、Windows原生amd64均完成构建；三平台的路径逃逸、符号链接/reparse、权限保护、进程树取消和stdio关闭达到等价安全结果，不以交叉构建、Rosetta、WOW或ARM runner替代。（验证：三个原生runner各运行native profile的7个固定package/test二元组20次及cross-build profile。）
- [ ] AC32 / F32：格式、静态、普通测试×3、race、敏感场景×20、coverage、三平台构建、repoaudit、docs、performance及E2E全部进入自动门禁；无missing、skip、超时或缓存旧结论。（验证：在同一revision运行final profile并检查每个Step与required manifest恰好出现批准次数。）
- [ ] AC33 / F33：F1–F33均可追溯到AC、Task和本Checklist；历史规格保留且current/historical/superseded关系无环；所有已勾选项有当前可复现证据。（验证：运行docs/repoaudit profile，期望双向追溯完整、无悬空替代、无计划冒充证据。）
- [ ] AC34 / N9：在batch追加、snapshot替换和状态发布各持久化barrier注入写失败或进程中断，重启后至少恢复最后成功revision，无半条记录覆盖旧状态。（验证：运行`TestProcessInterruptionAtCommitBarriersPreservesLastRevision`覆盖批准的四个barrier并比较磁盘状态。）
- [ ] AC35 / N10、N16：现有有效配置、权限规则、旧JSON、v1 JSONL和ExternalPath可直接使用或非破坏迁移；迁移失败时原文件内容、mode和位置保持不变。（验证：对成功/失败样本运行迁移与重启测试，并比较迁移前后原文件摘要。）
- [ ] AC36 / N12：代表性启动、普通对话、会话列表和只读工具四个workload相对固定治理前baseline均不超过20%非必要退化。（验证：每项5次预热、30次计分，按`s[14]+(s[15]-s[14])/2`计算中位数，并逐项要求`CurrentMedianNanos*5 <= BaselineMedianNanos*6`；ppm只展示不判定。）
- [ ] AC37 / N18–N21：整体覆盖率至少80.0%；普通测试在无真实凭据、无公网、无真实Provider环境连续3次通过；历史敏感并发/取消/关闭场景race重复20次通过。（验证：运行coverage、unit、race和hermetic检查，核对环境、次数、required结果及无skip。）

## 端到端场景

- [ ] AC38.1：授权碰撞不能复用授权，直接/间接权限文件绕过均被拦截，受保护文件不变。（验证：运行`TestE2EAC38AuthorizationCollisionAndPermissionProtection`，期望pass且无canary泄露。）
- [ ] AC38.2：大输出被安全截断并生成私有artifact，取消后完整进程树退出且不再产生副作用。（验证：运行`TestE2EAC38LargeOutputArtifactAndProcessTreeCancellation`，期望pass并核对artifact权限和2秒清理边界。）
- [ ] AC38.3：长会话完成保存、重启、恢复、外置和会话切换后，消息、工具状态、摘要、元数据及外置状态语义一致。（验证：运行`TestE2EAC38LongConversationRestartExternalizeAndSwitch`，期望pass并比较重启前后状态摘要。）
- [ ] AC38.4：MCP/Provider secret回显、恶意重定向、超限SSE、usage与关闭流程均不泄密、不死锁、不遗留网络、进程或goroutine资源。（验证：运行`TestE2EAC38MCPProviderSecretsRedirectBudgetAndClose`，期望pass且全渠道canary扫描为零。）
- [ ] AC38.5：macOS、Linux、Windows原生amd64均通过核心安全场景；错误平台、翻译执行、skip或仅交叉编译不能计为通过。（验证：三个原生runner分别运行`TestE2EAC38NativePlatformSecurity`及native identity gate。）
- [ ] AC38 汇总：E2E profile只通过唯一生产组装根运行，3个harness根、2个production-adapter/TrustedRoots根和上述5个AC38根各恰好pass一次，无额外替代、missing或skip。（验证：运行e2e profile并由required-test gate核对10个根测试。）

## 架构与集成约束

- [ ] I1 生产启动只有一个Assembly/Runtime registry；CLI、E2E与测试生产adapter复用同一组装根，不存在旧factory、第二组装根或双关闭表。（验证：运行assembly静态零引用和production-adapter/TrustedRoots测试。）
- [ ] I2 配置Resolve、秘密注册、SafeFS/Process/Artifact/网络owner、权限/工具、本地资源、Provider/Hook/MCP/Store、Context/Orchestrator、App/TUI按批准顺序组装；任一初始化失败按反向顺序幂等回滚。（验证：逐故障点注入并比较创建/关闭Trace，期望每个owner恰好关闭一次。）
- [ ] I3 SafeText、RuntimeRedactor、BoundedSink和安全DTO是跨界面、事件、模型、持久化及诊断的唯一文本边界，不存在raw fallback。（验证：运行AST/引用审计和全渠道canary测试。）
- [ ] I4 SafeFS、proctree和netpolicy由组装根签发窄capability/client；业务模块不能自行按字符串路径、裸exec或默认HTTP client绕过。（验证：运行依赖方向和禁止API静态审计及三平台原生安全测试。）
- [ ] I5 Provider ChatStream、MCP Manager/Connection/Transport、Conversation Store和App runtime均有唯一owner、双context取消边界及幂等Close；部分初始化、并发Close和消费者提前退出均可收敛。（验证：运行生命周期race矩阵和反向关闭Trace。）
- [ ] I6 Conversation v2、Orchestrator序号槽和App三层状态在保存、工具结果提交、usage提交及导航中保持确定顺序，迟到事件不能跨request/conversation边界。（验证：运行状态摘要golden、有序提交、导航事务及stale event测试。）

## CI、删除与证据闭环

- [ ] D1 最终CI只有冻结的`quality` matrix job，step 0/1/2/3依次为三个批准的commit-pinned action与checker；`.github/xagent-actions.lock.json`绑定实际workflow blob、三项manifest digest和完整无环依赖图。（验证：运行workflow strict parser、RootUses/TargetIdentity和dependency digest测试，期望无动态uses、额外节点或网络重取。）
- [ ] D2 Rpre在三个pending marker形态下取得11个job绿色证据；checkout OID、CommandManifest、workflow dependency digest和provider identity均一致。（验证：导入rpre safe evidence后核对ExpectedEvidenceManifest对应连续子序列及ledger闭集。）
- [ ] D3 D01–D17均为唯一单父连续提交，每个delta只含批准的单文件删除和候选状态单步前移；每阶段在下一删除前完成指定job及typed evidence导入。（验证：重建ancestry、17个exact diff和Prefix01–Prefix17；期望64个删除阶段job恰好一次、无跨stage预取或拼接。）
- [ ] D4 R0等于D17或其唯一`go.mod`/`go.sum` tidy child；固定离线`go mod tidy -diff`可复演，R0的11个job、after_r0 preservation和86个pre-E job完整。（验证：核对R0TransitionEvidence、module diff/digest、R0 job与pre-E ledger计数。）
- [ ] D5 E是R0唯一单父，delta恰好为README、docs index和本Checklist三个canonical marker pass block，marker外无byte变化且内容只绑定R0；E的11个job与after_e preservation均通过。（验证：运行marker updater/delta/docs审计和post-E importer，期望ExpectedEvidenceManifest 97个job恰好一次。）
- [ ] D6 Attestation由完整typed ledger生成并独立verify-record；可选external readback只接受批准的两帧或完全不存在，不联网且不保存raw response。（验证：运行generate、verify-record及所选readback分支的codec/digest/关系复验。）
- [ ] D7 Archive严格经过preparation→二次全量复验→manifest删除→finalization→只读化；manifest只在第二次复验成功后删除，ledger/archive、lease、active和control root不被当前治理目标删除。（验证：运行finalize-archive与verify-archive中断矩阵，核对四种合法phase、闭集、digest和POSIX/Windows只读权限。）

## 最终验收结论

- [ ] Z1 R0上的AC1–AC38全部有当前safe evidence，E上的97个ExpectedEvidenceManifest job全部通过，attestation与只读archive复验通过；仓库精确停留在E，用户本地数据未删除、未移动、未覆盖，Git历史未重写。（验证：在E fresh checkout运行final、commit-repoaudit、docs、三平台native/e2e、performance及attestation/archive复验，结果全部pass后方可勾选。）
- [ ] Z2 无未解释的失败、skip、missing、extra、不同revision拼接、悬空证据或超出Spec范围的实现；任何不通过项均保持未勾选并记录实际结果与修复任务。（验证：汇总机器门禁和人工验收记录，期望未通过列表为空且追溯审计exit 0。）
