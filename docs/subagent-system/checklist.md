# 子 Agent 委派与后台任务系统 Checklist

> 每项都必须通过运行测试、观察事件/Provider 请求或操作 TUI 来验证。完成开发后记录实际证据；仅阅读代码不算通过。

## 实现完整性与验收标准

- [ ] **AC1（F1–F4）统一入口**：分别从模型 Agent 工具和 TUI `/agent` 提交 Defined 任务；验证两者的参数校验、任务记录、事件序列和终态 schema 一致。提交空任务、非法类型或未知角色；运行 `go test ./internal/subagent ./internal/orchestrator ./internal/command`，确认返回结构化可恢复错误且任务数/队列数不增加。
- [ ] **AC2（F4、F14）Fork 强制后台**：请求 Fork 前台；观察任务立即为 background，父会话继续可输入，首次子请求只使用委派瞬间快照。
- [ ] **AC3（F5–F7）角色格式**：加载合法角色并查看目录、用途和每轮角色正文；加入空名称、未知字段、非法模型/权限、负轮次和空正文；确认无效并有脱敏诊断。
- [ ] **AC4（F8–F10）来源覆盖与原子刷新**：在 project/user/builtin/plugin 放置同名和不同名角色；确认覆盖顺序 project > user > builtin > plugin。同层有效重名使首次加载失败；运行中刷新失败时旧快照和旧任务不变。
- [ ] **AC5（F9）坏高层回退**：高优先级同名定义损坏、低优先级有效；确认低层角色继续生效并出现安全诊断。
- [ ] **AC6（F10–F12）快照和模型/权限解析**：发布快照后修改角色；确认运行任务正文不变。指定未映射模型或放宽权限；确认准备失败且全局配置不变。
- [ ] **AC7（F13）Defined 上下文**：用捕获 Provider fixture 检查请求包含标准系统/安全指令、项目指令、角色正文和任务内容，且不含父消息、父 Skill Activity 或激活 Skill。
- [ ] **AC8（F14、N7）Fork 快照与缓存**：在父请求产生 Agent 调用后捕获 Fork 请求；检查父消息、系统块、过滤后工具、模型和缓存策略的稳定前缀。父会话随后新增消息或刷新配置；确认子请求不可见。
- [ ] **AC9（F15–F16）运行时隔离**：并行运行两个任务，分别修改消息、确认状态、ReadCache 和用量；确认任何变化不出现在另一任务或主会话，Provider/Hook/文件系统仍可并发服务。
- [ ] **AC10（F17）权限不继承**：父会话已有临时授权；子任务调用同一危险工具时重新确认。子任务允许一次/会话后，父任务后续调用仍按父规则决策。
- [ ] **AC11（F18）递归拒绝**：让子模型伪造 Agent 调用；确认在能力预检或 system route 前返回 recursive_delegate，且无新任务、队列项或 Provider 请求。
- [ ] **AC12（F19–F20）非交互循环**：使用多轮工具 fixture，确认持续执行至无工具调用并正常完成；达到 global/role 较小轮次上限后不再请求 Provider，并发布 limit_reached 原因。
- [ ] **AC13（F21）确认暂停**：危险工具请求使任务进入 waiting_confirmation；TUI 显示 TaskID 和脱敏参数。分别允许、拒绝和取消；确认允许后继续，拒绝不执行工具并产生结构化结果。
- [ ] **AC14（F22–F23）错误/取消/关闭**：注入 Provider 流错误、工具致命错误、任务取消和应用关闭；确认停止后不启动新模型/工具，已产生安全文本保留、错误脱敏且进程不崩溃。
- [ ] **AC15（F24–F27）多层工具收窄**：设置父集合、角色 allow/deny、全局禁止、Plan/Hook/权限和后台配置冲突；确认前台不提前应用后台白名单，真实后台才应用；过滤工具不进 Provider 定义，伪造调用被拒绝。
- [ ] **AC16（F26）后台默认/扩展白名单**：默认后台下 Write/Edit/Bash 不可见且伪造调用不执行；显式扩展后危险工具仍经过既有确认和权限链，不因后台自动放行。
- [ ] **AC17（F28、N19）并发/队列上限**：高负载提交超过并发 4 或队列 32；确认立即得到 queue_full，实际运行/排队数不超配置，递归和超限请求不创建隐藏任务。
- [ ] **AC18（F29）任务详情**：Defined/Fork 任务详情显示稳定 ID、角色 provenance、类型、来源、时间、状态、停止原因、摘要、错误和用量；状态仅沿合法状态机转移。
- [ ] **AC19（F30）三种进入后台方式**：分别验证显式后台、首轮超过 10 秒自动后台、TUI 手动切后台；比较 TaskID、事件序号、工具调用和迭代进度，确认不重启、不重复执行。
- [ ] **AC20（F31）父取消解耦**：任务切后台后取消父请求并继续普通输入；确认子任务继续运行，只有任务自身取消或应用关闭才停止。
- [ ] **AC21（F32）配置生效与 fail-closed**：修改并发/队列配置并重新启动；确认下一次调度使用新上限，负数、零值不允许字段和溢出配置在启动时拒绝。
- [ ] **AC22（F33、N11–N13）机器事件流**：不连接 TUI 消费事件；确认每项带 TaskID、全局 Revision、任务 Sequence，覆盖文本/思考、工具、确认、进度、用量和终态，且前后台切换无丢失/重复/矛盾终态。慢订阅者收到 Gap 后可用 Watermark + List/Get 重同步。
- [ ] **AC23（F34）运行中结果回灌**：后台完成后确认主 Agent 收到固定 `subagent_result` schema；主 Agent 运行中只在下一安全回灌边界看到 TaskID、状态、摘要、停止原因、用量和安全错误。
- [ ] **AC24（F34、N21）主轮结束后的通知**：主 Agent 先结束再让子任务完成；确认结果进入有界待消费 Inbox，下一请求可 Claim，但系统不自动启动额外模型回合。
- [ ] **AC25（F35–F37）TUI 任务操作**：在 TUI 查看列表/详情、处理确认、手动切后台、取消和查看成功/失败结果；不存在、过期或非 owner 操作返回明确错误。
- [ ] **AC26（F36）主会话投影**：消费结果后检查会话 JSONL 和 Provider Context；确认只出现结构化结果或脱敏摘要，思考、临时消息和完整工具轨迹不进入主会话。
- [ ] **AC27（F38、N14）脱敏与限长**：在角色正文、任务参数、模型响应、工具输出和摘要中注入模拟 key/token/password 及超长内容；检查目录、事件、诊断、详情、通知和会话记录，确认秘密不出现且截断有标记。
- [ ] **AC28（N15）普通路径回归**：不创建子 Agent，运行普通对话、Skill、Plan Mode、权限确认、Memory、MCP 和会话恢复；与基线测试结果比较，无行为变化。
- [ ] **AC29（N16）模型选择隔离**：并行提交不同模型别名任务；捕获 Provider 请求确认模型只影响对应任务，父会话、其他任务和后续默认请求不变。
- [ ] **AC30（N20）可测试性与 TUI E2E**：在无真实 Provider、无完整 TUI 环境完成角色、上下文、过滤、权限、后台、取消、队列和回流 fixture；再通过真实 TUI 完成一条端到端场景。
- [ ] **AC31（N21–N22）范围边界**：应用关闭/重启时未完成任务不标记成功、不跨进程恢复；检查工作区没有 Worktree、团队编排或跨会话后台记录，也没有新 Provider 协议/网络工具。

## 集成检查

- [ ] **I1 组装顺序**：运行 assembly 测试，确认工具注册全部完成后才 `Registry.Seal`，再创建 ModelCatalog、角色 Manager、RunnerFactory 和 TaskManager。
- [ ] **I2 单一提交路径**：静态引用审计和模型/TUI 集成测试确认两种入口都只调用 `subagent.Service.Submit`，不存在第二套独立任务创建函数。
- [ ] **I3 Registry 一致性**：Provider 请求工具定义、CapabilitySet 预检和 ScopedExecutor 最终重检来自同一 sealed View；验证不存在“模型不可见但可执行”或反向情况。
- [ ] **I4 Hook 隔离**：Hook 生命周期测试确认每个任务使用 `subagent:<TaskID>` SessionID，SessionStart/BeginTurn/EndTurn/SessionEnd 配对且父 Session prompt 不串入。
- [ ] **I5 Completion 单一来源**：比较 TaskManager、terminal Event、ResultInbox、TUI 和主会话投影的状态、摘要、停止原因和用量，确认均由同一 Completion 深拷贝而来。
- [ ] **I6 关闭顺序**：生命周期测试确认停止准入、取消并等待子任务、关闭任务 Hook 后才关闭普通交互，最后关闭 Hook/MCP/Provider；重复 Shutdown 幂等。
- [ ] **I7 敏感边界**：跨模块错误、事件、诊断、JSONL 和 Provider adapter 测试确认只传递 SafeError/SafeDiagnostic 和 bounded 安全字段。
- [ ] **I8 配置 presence**：配置集成测试确认 Optional 只在 Resolve 阶段解释一次，运行层不自行补默认值或把显式空值当缺失。

## 编译、测试与质量门

- [ ] **Q1 格式与静态检查**：运行 `gofmt -w` 仅处理实现文件后，`go vet ./...` 无新增诊断。
- [ ] **Q2 单元测试**：运行 `go test ./...`，所有新增与既有单元测试通过。
- [ ] **Q3 并发检查**：运行 `go test -race ./internal/agentrole ./internal/tool ./internal/permission ./internal/subagent ./internal/orchestrator`，无 data race。
- [ ] **Q4 资源边界**：运行 limits/负载 fixture，确认文件、候选、事件、结果、缓存、队列、tombstone 和摘要均在配置上限内，不发生整数溢出或无界分配。
- [ ] **Q5 失败注入**：运行 Provider 流错误、工具错误、确认超时、取消、关闭和 Inbox 满载测试，确认进程不崩溃且每种结果有固定机器码。
- [ ] **Q6 文档一致性**：检查 `spec.md` 每条 F/N、`plan.md` 每个组件和 `task.md` 每个任务都有对应清单或验证；无占位标记、旧接口名称或未配对 Markdown fence。

## 端到端场景

- [ ] **E1 Defined 前台→自动后台→完成**：TUI 提交 Defined 前台任务，首轮超过阈值后自动 Detach；父对话继续输入，任务详情持续更新，最终显示一次脱敏完成通知。
- [ ] **E2 Fork 快照与结果回流**：模型提交 Fork 并请求前台；确认任务直接后台运行，父会话随后新增内容对子任务不可见；下一安全 Provider 回合收到一次固定 `subagent_result`。
- [ ] **E3 等待确认与取消**：后台任务请求危险工具，TUI 显示带 TaskID 的确认卡；拒绝后任务继续或按模型逻辑完成，取消时 Broker 唤醒且不再启动新工具/模型。
