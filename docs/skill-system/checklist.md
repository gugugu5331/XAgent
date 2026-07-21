# Skill System Checklist

> 每一项都通过运行代码、自动化测试或观察真实 TUI 行为验证。代码尚未实现前保持未勾选；验收阶段记录实际命令输出和观察结果后再勾选。

## Skill 格式与发现

- [x] C01：合法单文件 Skill 能读取全部元信息和非空 SOP，并把文件所在目录作为规范包根。（验证：运行 `go test ./internal/skill -run 'TestParse.*Valid|TestDiscoverSingleFiles'`，期望合法定义字段与输入一致。）
- [x] C02：合法目录型 Skill 只把 `SKILL.md` 作为入口；同目录模板、示例、脚本和参考 Markdown 不会自动成为 Skill 或进入 SOP。（验证：运行 `go test ./internal/skill -run TestDiscoverDirectoryPackages`，期望目录只产生一个定义且正文不含辅助文件内容。）
- [x] C03：空名称、空说明、非法名称、未知字段、未知模式、负 history、shared 非零 history、空工具项和空正文均被跳过并产生可定位的脱敏诊断。（验证：运行 `go test ./internal/skill -run 'TestValidateMetadata|TestParse.*Invalid'`，期望每类输入均有明确失败结果。）
- [x] C04：单个文件损坏、不可读或超限时不会阻断其他有效 Skill；损坏的高优先级定义不会遮蔽低层有效同名定义。（验证：运行 `go test ./internal/skill -run 'TestDiscover.*Invalid|TestManager.*InvalidOverride'`，期望有效目录仍完整。）
- [x] C05：builtin、user、project 同名时依次由 project、user、builtin 生效，结果不受目录遍历和文件创建顺序影响。（验证：运行 `go test ./internal/skill -run 'TestBuildSnapshotPrecedence|Test.*Deterministic'`，期望三次移除覆盖层后来源按顺序回退。）
- [x] C06：同一来源存在两个有效规范同名 Skill 时，首次加载失败；运行中制造相同冲突时旧 generation、目录和命令保持不变。（验证：运行 `go test ./internal/skill ./internal/app -run 'Test.*SameTierConflict|TestSkillCommandRefresh'`，期望启动错误和刷新回滚均可观察。）
- [x] C07：最终生效 Skill 引用不存在工具时，首次加载失败；热更新加入未知工具时拒绝候选并保留旧快照。（验证：运行 `go test ./internal/skill ./cmd/xagent -run 'Test.*UnknownTool|TestStartupSkillAssembly'`，期望错误发生在接受输入前。）
- [x] C08：每个来源最多 256 个入口、入口最大 256 KiB、展开 SOP 最大 128 KiB、参数最大 32 KiB、名称最大 64 字符。（验证：运行 `go test ./internal/skill -run 'Test.*Limit|Test.*TooLarge'`，期望边界内通过、超过边界按规格失败或跳过。）

## 两阶段加载与共享模式

- [x] C09：没有活动 Skill 时，捕获的 Provider 请求只包含有效 Skill 的规范名称和一句说明，不包含 SOP、模板、示例、脚本或参考资料。（验证：运行 `go test ./internal/prompt ./internal/orchestrator -run 'TestSkill.*Catalog|TestProfileChatRequest'` 并检查捕获请求。）
- [x] C10：`load_skill` 始终出现在 Provider 工具定义中，即使活动白名单为空、极窄或当前处于 Plan Mode；普通 Executor 不能绕过 Orchestrator 直接执行它。（验证：运行 `go test ./internal/tool ./internal/orchestrator -run 'TestLoadSkillTool|TestRegistryView|TestPrepareSkill'`。）
- [x] C11：SOP 中所有 `{{args}}` 由完整参数做一次字面替换；参数自身包含 `{{args}}` 时不递归展开，其他占位符不被解释。（验证：运行 `go test ./internal/skill -run TestActivityTemplateExpansion`，期望展开文本逐字匹配。）
- [x] C12：共享 Skill 经短命令调用后立即执行，并在首轮 Provider 请求中出现完整活动 SOP。（验证：运行 `go test ./internal/app -run TestExecuteSharedSkillCommand`，检查首次捕获请求。）
- [x] C13：共享 Skill 经 Agent 的 `load_skill` 调用后从下一 iteration 生效；工具结果后的请求和后续各轮持续携带同一 SOP。（验证：运行 `go test ./internal/orchestrator -run TestLoadSharedSkillAtIterationBoundary`，逐轮比较系统区块。）
- [x] C14：多个共享 Skill 按规范名称稳定排序；重复激活同名 Skill 使用最新有效 Definition 和参数原子替换旧快照。（验证：运行 `go test ./internal/skill -run 'TestActivity.*Ordering|TestActivity.*Replace'`。）
- [x] C15：执行 `/name args` 后主历史保存原始命令和完整参数，不保存展开后的 SOP；助手回复和普通工具历史继续保存在主会话。（验证：运行 `go test ./internal/app -run TestExecuteSharedSkillCommand`，检查 Conversation 消息角色与内容。）
- [x] C16：热更新修改或删除已激活 Skill 时，当前 Activity 继续使用激活时的 SOP、模型和白名单；重新激活后才使用新版本。（验证：运行 `go test ./internal/app -run TestActiveSkillSnapshotSurvivesRefresh`，比较刷新前后 Provider 请求。）
- [x] C17：共享 Skill 加载失败、参数超限或模型冲突时不产生部分活动状态，也不追加伪造的用户消息。（验证：运行 `go test ./internal/skill ./internal/app -run 'TestActivityAtomicActivation|TestExecuteSharedSkill.*Failure'`。）

## 独立执行

- [x] C18：独立 Skill 只激活目标 Skill，不继承主会话已活动的共享 Skill。（验证：运行 `go test ./internal/orchestrator -run TestBuildIndependentConversation`，检查临时 Profile 的活动列表。）
- [x] C19：独立上下文只携带最近 N 个已完成用户轮次，并完整保留轮内 assistant、tool_call 和 tool_result，不从工具链中间截断。（验证：运行 `go test ./internal/skill ./internal/orchestrator -run 'TestRecentCompleteTurns|TestBuildIndependentConversation'`。）
- [x] C20：history=0 时不带主历史；N 大于现有完整轮次数时只带现有完整轮次；尾部未完成轮次不被复制。（验证：运行 `go test ./internal/skill -run TestRecentCompleteTurns`。）
- [x] C21：独立执行复用当前权限规则；危险工具仍出现确认提示，允许、拒绝和取消决定沿用现有语义。（验证：运行 `go test ./internal/orchestrator -run TestIndependent.*Permission`，期望相同确认对象和结果状态。）
- [x] C22：独立执行期间，流式文本、思考和工具进度实时可见；结束后临时轨迹从 MessagesView 消失。（验证：运行 `go test ./internal/tui ./internal/app -run 'TestTransientMessages|TestExecuteIsolatedSkillCommand'`。）
- [x] C23：独立执行成功后主历史只新增原始 Skill 命令和最终回复；没有临时工具轨迹、思考或第二次摘要模型调用。（验证：运行 `go test ./internal/orchestrator ./internal/app -run 'TestRunIndependentNoSideEffects|TestExecuteIsolatedSkillCommand'`，断言 Provider 调用次数和消息序列。）
- [x] C24：独立执行取消、超时、Provider 失败或没有完整最终回复时，不写入成功摘要、不继续后台执行并恢复可输入状态。（验证：运行 `go test ./internal/orchestrator ./internal/app -run 'TestIndependent.*Cancel|TestIndependent.*Timeout|TestIndependent.*Failure'`。）
- [x] C25：独立上下文再次加载 isolated Skill 得到明确可恢复错误；加载 shared Skill 只更新临时 Activity，不污染主 Activity。（验证：运行 `go test ./internal/orchestrator -run TestIndependentNestedSkillRules`。）
- [x] C26：独立临时会话不写 Store、不进入会话列表、不直接更新 Memory，也不修改主 Conversation 原有消息对象。（验证：运行 `go test ./internal/orchestrator -run TestRunIndependentNoSideEffects`，检查 fake Store、Memory 和主消息深拷贝。）

## 模型、工具与资源安全

- [x] C27：共享 Skill 指定模型后，各后续 Provider 请求都使用该模型；清除 Activity 后恢复应用默认模型。（验证：运行 `go test ./internal/provider ./internal/orchestrator ./internal/app -run 'Test.*ModelOverride|TestClearSkillsKeepsConversation'`。）
- [x] C28：独立 Skill 的模型覆盖只在临时请求中生效；后续主会话和其他请求仍使用默认模型。（验证：运行 `go test ./internal/provider ./internal/orchestrator -run 'Test.*Model.*Isolation|TestRunIndependent.*Model'`。）
- [x] C29：激活一个指定不同非空模型的第二共享 Skill 时失败，已有活动集、模型和工具集合完全不变。（验证：运行 `go test ./internal/skill ./internal/orchestrator -run 'TestActivity.*ModelConflict|TestLoadShared.*Conflict'`。）
- [x] C30：多个活动白名单取交集；未声明或空白名单不额外限制；结果永远是基础 Registry 的子集并额外保留 `load_skill`。（验证：运行 `go test ./internal/skill ./internal/tool -run 'TestBuildProfile|TestRegistryView'`。）
- [x] C31：Plan Mode 继续与 Skill 白名单求交；Write、Edit、Bash 不向模型暴露，也不能通过伪造工具调用进入 Executor。（验证：运行 `go test ./internal/skill ./internal/orchestrator -run 'TestBuildProfile.*Plan|TestFilteredToolCannotExecute'`。）
- [x] C32：允许的危险工具继续经过 Authorizer 和用户确认，Skill 白名单不会赋予免确认权限。（验证：运行 `go test ./internal/orchestrator -run 'TestFilteredToolCannotExecute|TestIndependent.*Permission'`，检查确认事件。）
- [x] C33：目录型 Skill 可通过允许的 Read、Glob、Grep 读取包内辅助资源；未在白名单中的读取工具不可调用。（验证：运行 `go test ./internal/tool ./internal/orchestrator -run 'Test.*ExtraRoot|TestProfileChatRequest'`。）
- [x] C34：Skill 资源根不能读取包外目标或通过 symlink 越界；Write、Edit 和 Bash 不因额外只读根获得项目外写权限。（验证：运行 `go test ./internal/tool -run 'TestReadScope|Test.*ExtraRoot'`，期望越界操作全部拒绝。）

## 命令、热更新与生命周期

- [x] C35：最终有效且不冲突的 Skill 自动出现在 `/help` 和 Tab 补全中，并显示 `Skill/shared` 或 `Skill/isolated`。（验证：运行 `go test ./internal/command ./internal/app ./internal/tui -run 'Test.*Badge|TestSkillHelpAndCompletion'`。）
- [x] C36：硬编码 review Prompt 和可见固定 review 命令已移除；内置 review Skill 提供 `/review`，隐藏 `/rv` 仍转发到当前有效 review Skill。（验证：运行 `go test ./internal/command ./internal/app -run 'TestReviewMigration|Test.*Review.*Skill'`。）
- [x] C37：与基础设施命令或别名冲突的 Skill 不注册短命令并产生诊断，但仍能由 Agent 使用 `load_skill` 激活。（验证：运行 `go test ./internal/skill ./internal/app ./internal/orchestrator -run 'Test.*ReservedCommand|TestPrepareSkill'`。）
- [x] C38：新增、修改或删除 Skill 后，下一次 Enter、Tab 或普通请求前刷新目录和动态命令，无需重启应用。（验证：运行 `go test ./internal/app -run TestSkillCommandRefresh`，分别触发三个交互边界。）
- [x] C39：无文件变化时热更新只比较缓存指纹，不重新解析正文或改变 generation。（验证：运行 `go test ./internal/skill -run TestManagerRefreshNoChange`，检查解析计数和 generation。）
- [x] C40：热更新中单文件解析错误只跳过该文件；同层冲突或未知工具导致整个候选被拒绝，旧目录、命令和活动状态保持一致。（验证：运行 `go test ./internal/skill ./internal/app -run 'TestManagerRefresh|TestSkillCommandRefresh'`。）
- [x] C41：`/clear` 继续只清空当前消息显示并保留 Conversation 历史，同时清除全部活动 Skill、模型覆盖和 Skill 工具限制。（验证：运行 `go test ./internal/app -run TestClearSkillsKeepsConversation`，比较清除前后历史并检查空 Activity。）
- [x] C42：新建会话、切换会话和应用重启后 Activity 均为空，不从会话 JSONL 恢复 Skill、模型或白名单状态。（验证：运行 `go test ./internal/app ./internal/conversation -run 'TestSkillActivityLifecycle|Test.*Conversation'`。）

## 内置样板与脱敏

- [x] C43：内置 commit 可发现、可补全、mode=shared、history=0、默认模型，工具恰为 Read/Glob/Grep/Bash，SOP 能检查改动、验证并创建提交。（验证：`TestBuiltinMetadataAndMaterialization` 断言元信息和提交 SOP 的必要步骤；构建产物 `/help` 冒烟验证可见性。完整交互见 E03。）
- [x] C44：内置 review 可发现、可补全、mode=isolated、history=1、默认模型，工具恰为 Read/Glob/Grep/Bash，SOP 要求只审查并按严重程度报告。（验证：`TestBuiltinMetadataAndMaterialization`、`TestReviewMigration` 和构建产物 `/help` 冒烟；完整交互见 E02。）
- [x] C45：内置 test 可发现、可补全、mode=isolated、history=1、默认模型，工具恰为 Read/Glob/Grep/Bash，SOP 能发现并运行测试后报告真实结果。（验证：`TestBuiltinMetadataAndMaterialization` 断言元信息和测试 SOP 的必要步骤；构建产物 `/help` 冒烟验证可见性。完整交互见 E04。）
- [x] C46：内置能力包通过嵌入资源工作，不依赖开发机源码路径；辅助资源物化失败不会部分激活，成功物化的临时资源可清理。（验证：运行 `go test ./internal/skill -run TestBuiltin`，并从构建出的二进制启动一次。）
- [x] C47：Skill 路径、frontmatter、参数、解析错误、工具结果和独立摘要中的模拟 API Key、Token、密码不会原样出现在诊断、状态栏或消息输出。（验证：运行 `go test ./internal/skill ./internal/app ./cmd/xagent -run 'Test.*Redact|TestStartupSkillAssembly'`，搜索原始 canary 应为零命中。）

## 集成与回归

- [x] C48：没有有效 Skill 或没有活动 Skill 时，普通 Provider 请求的模型、工具、消息、Prompt 固定区块和事件顺序与引入 Skill 前等价。（验证：`TestEmptySkillCatalogPreservesOrdinaryRequestBehavior`、`TestBuildWithoutSkills` 及既有 Agent Loop 回归测试。）
- [x] C49：现有命令、会话保存与恢复、上下文压缩、Memory、MCP、权限确认和 Plan Mode 自动化测试全部保持通过。（验证：运行 `go test ./internal/command ./internal/conversation ./internal/contextmgr ./internal/memory ./internal/mcpclient ./internal/permission ./internal/orchestrator ./internal/app`。）
- [x] C50：所有变更 Go 文件格式正确，补丁没有尾随空白或冲突标记。（验证：运行 `gofmt -l` 检查变更 Go 文件，期望无输出；运行 `git diff --check`，期望退出码 0。）
- [x] C51：静态检查无错误。（验证：运行 `go vet ./...`，期望退出码 0。）
- [x] C52：全项目自动化测试通过且不访问真实网络或真实 Provider。（验证：运行 `go test ./...`，期望所有包返回 `ok`。）
- [x] C53：当前源码完整构建成功，内置 Skill 随二进制可用。（验证：运行 `go build ./...`，期望退出码 0；启动构建产物后 `/help` 能看到 commit、review、test。）
- [x] C54：Manager、Activity、Orchestrator 和 App 的并发路径无已检测数据竞争。（验证：运行 `go test -race ./internal/skill ./internal/orchestrator ./internal/app`，期望退出码 0。）
- [x] C55：所有公开新增接口至少有真实调用方，不存在只为测试存在的死接口。（验证：运行 `go vet ./...`、`go test ./...` 和 `rg` 检查新增导出符号的生产调用点，期望每项均可定位。）

## 真实 TUI 端到端场景

### E01：目录轻量注入与按需加载

- [x] 启动带捕获请求能力的本地测试 Provider，放置一个正文含唯一 canary 的项目 Skill；发送普通消息时首个请求只能看到名称和说明，Agent 调用 `load_skill` 后的下一请求才出现 canary SOP。（实际：真实 TUI 产生恰好两次请求；请求 1 含名称与说明但不含 `E01_FULL_SOP_CANARY_7d519a`，请求 2 才含完整 SOP、替换后的 `from-agent` 参数，并把工具收窄为 `Read`、`load_skill`。）

### E02：`/review` 独立运行

- [x] 先完成一轮含工具调用的主对话，再执行 `/review 当前改动`；TUI 实时显示审查进度和正常权限确认，完成后重载会话只看到原始 `/review` 和最终审查摘要，看不到临时工具轨迹。（实际：Bash 从“等待确认”经 `y` 到“执行中”和成功结果均实时可见；会话 JSONL 角色恰为 `user,tool_call,tool_result,assistant,user,assistant`，重载后有 `/review 当前改动` 与 `E02_REVIEW_FINAL`，无独立 Bash 轨迹。）

### E03：`/commit` 共享激活

- [x] 在有可提交改动的测试仓库执行 `/commit 示例提交`；首轮即出现 commit SOP，后续工具轮持续存在，工具只限 Read/Glob/Grep/Bash 加 `load_skill`，主历史保存原短命令。（实际：四次请求均含 commit SOP，工具集始终恰为 `Bash,Glob,Grep,Read,load_skill`；三次 Bash 均经过权限确认，真实生成提交 `0c4f2c6 e2e skill commit`，会话首条保留原始 `/commit 示例提交`。）

### E04：`/test` 独立执行

- [x] 执行 `/test 运行相关测试`；临时上下文只携带最近一轮完整历史，实际运行测试并实时显示工具状态，完成后只回流最终结果摘要且不创建额外会话。（实际：独立请求只携带最近一个完整 user/assistant 轮次，确认后真实执行 `go test ./...` 并显示 `ok e04fixture`；唯一会话 JSONL 仅四条主历史消息、无 Bash 轨迹，Context Store 文件数为 0，重载仍只有一个会话。）

### E05：模型冲突与工具交集

- [x] 创建两个 shared 项目 Skill：先验证相同模型的工具白名单取交集，再把第二个改为不同模型并重新激活；第二次激活应失败且第一个 Skill 的 SOP、模型和工具集保持原样。（实际：第二个同模型 Skill 激活后工具交集为 `Read,load_skill`；改成冲突模型后激活显示 `active skills require different models` 且没有 Provider 请求，随后请求仍使用原模型、两份旧 SOP 与原工具交集。）

### E06：热更新与活动快照

- [x] 激活一个 shared Skill 后修改其说明、SOP 和 mode；下一次 Tab/Enter 看到新目录与命令元数据，但当前活动请求仍使用旧 SOP；重新调用后才使用新 SOP。（实际：热更新后 `/help` 立即显示新说明与 `[Skill/isolated]`；普通请求的目录已更新但活动区仍只有旧 SOP，重新执行 `/hot ARG_NEW` 后独立请求只含新 SOP。）

### E07：`/clear` 与会话生命周期

- [x] 激活多个 shared Skill 后执行 `/clear`；消息显示清空、历史消息数量不变，下一请求恢复默认模型和基础工具。随后新建、切换会话和重启，均没有活动 Skill。（实际：清除前后 `/session` 消息数均为 4，显示与 Skills 状态清空；下一请求恢复默认模型和全部基础工具。新会话、重启及从启动历史列表装载已有会话后均无活动 Skill。）

### E08：无效白名单启动失败

- [x] 在项目 Skill 中声明不存在的工具后启动应用；程序应在 TUI 接受输入前退出并给出脱敏错误。修正为已注册工具后重新启动应成功。（实际：未知工具使用 API-key canary 时进程在 alternate-screen 前以 1 退出，stderr 只显示 `unknown tool "[redacted]"`；改为 `Read` 后真实 TUI 出现并以 0 退出。）

### E09：保留命令冲突

- [x] 新建名为 `clear` 或基础命令别名的项目 Skill；`/help` 和 Tab 不出现该 Skill 命令，诊断说明冲突，但 Agent 通过 `load_skill` 仍能加载它。（实际：诊断为 `skill_command_reserved`，`/help` 仅有基础 `/clear`，`/cl<Tab>` 只补成 `/clear`；普通请求中 Agent 成功调用 `load_skill(clear)`，下一请求含保留 Skill 的 SOP 与参数并把工具收窄为 `Read,load_skill`。）

### E10：资源根与路径越界

- [x] 创建目录型 Skill，放置一个包内参考文件和一个指向包外的 symlink；允许的 Read 可读参考文件，symlink 读取和对包外路径的 Write/Edit 均被拒绝。（实际：包内 Read 成功；symlink Read、包外 Write、包外 Edit 均返回 `permission_denied`。外部原文件前后 SHA-256 同为 `7dc3b41e...60a8`，目标新文件不存在。）

## 验收记录（2026-07-22）

- 自动化条目 C01–C55 已完成；以下命令均通过：
  - `go test ./... -count=1`
  - `go vet ./...`
  - `go build ./...`
  - `go test -race ./internal/skill ./internal/orchestrator ./internal/app ./internal/tool ./internal/permission -count=1`
  - `gofmt -l internal cmd/xagent`（无输出）
  - `git diff --check`
- 一次并行高负载全量测试中，MCP stdio 注册烟测在 2.7 秒期限内超时；随后独立重复运行 `go test ./internal/mcpclient -run TestMCPStdioToolRegistersAndExecutesThroughToolExecutor -count=10` 全部通过，顺序全量测试也通过，未复现功能故障。
- 构建真实 `xagent` 二进制并进入 TUI 执行 `/help`，确认 `/commit [Skill/shared]`、`/review [Skill/isolated]`、`/test [Skill/isolated]` 已动态注册，正常退出且退出码为 0。
- 使用 `rg` 审计新增导出 API，已移除仅供测试使用的 `Parse`、`CatalogPrompt`、`ActivePrompt` 包装函数；测试改走实际生产入口。
- E01–E10 已在隔离临时项目中使用真实 `xagent` 二进制、PTY、本地 OpenAI-compatible Provider 和捕获的请求 JSONL 全部执行通过；所有 XAgent 进程正常场景均以 0 退出。请求、会话和配置等证据位于 `/private/tmp/xagent-e2e-e01e04.oLg8Bk`、`/private/tmp/xagent-e05e07-refresh-clear-v1`、`/private/tmp/xagent-e08e10.h3oUa0`，关键 PTY 观察已逐项摘录在上方。
- E04 首轮还暴露了 `memory.enabled: false` 未接入生产 wiring 的独立问题；修复后以含“记住”标记的真实 TUI 请求复验，主请求完成后等待 5 秒仍只有一次 Provider 请求，且未再触发 panic 或记忆抽取。复验证据位于 `/private/tmp/xagent-memory-disabled.xN4xIf`。

## 验收标准覆盖

| Spec 验收标准 | Checklist 条目 |
| --- | --- |
| AC1 | C01、C02 |
| AC2 | C03、C08 |
| AC3 | C05 |
| AC4 | C04 |
| AC5 | C06 |
| AC6 | C07、E08 |
| AC7 | C09、E01 |
| AC8 | C10 |
| AC9 | C11 |
| AC10 | C12、C13、C14 |
| AC11 | C15、E03 |
| AC12 | C18、C19、C20 |
| AC13 | C21、C22、C23、E02 |
| AC14 | C23、C24 |
| AC15 | C27、C28、C29、E05 |
| AC16 | C30、C31、C32、E05 |
| AC17 | C35、C36、C37、E09 |
| AC18 | C16、C38、C39、E06 |
| AC19 | C04、C06、C07、C40 |
| AC20 | C17、C41、C42、E07 |
| AC21 | C43、C44、C45、E02、E03、E04 |
| AC22 | C33、C34、E10 |
| AC23 | C47 |
| AC24 | C25 |
| AC25 | C48、C49 |
| AC26 | C50、C51、C52、C53、C54、C55 |
| AC27 | E01–E10 |

全部 AC1–AC27 均已由至少一个可运行或可观察条目验证；自动化条目 C01–C55 与真实 TUI 条目 E01–E10 均已完成。
