# Skill System Tasks

## 文件清单

| 操作 | 文件 | 职责 |
| --- | --- | --- |
| 新建 | `internal/skill/types.go` | Skill 元数据、定义、目录、快照、调用类型与固定边界 |
| 新建 | `internal/skill/parser.go` | 严格 YAML frontmatter、正文与字段校验 |
| 新建 | `internal/skill/discovery.go` | 单文件和目录型 Skill 发现、路径规范化与文件限制 |
| 新建 | `internal/skill/snapshot.go` | 三级覆盖、同层冲突、工具校验与短命令资格计算 |
| 新建 | `internal/skill/manager.go` | 首次严格加载、文件指纹、有效快照与原子热更新 |
| 新建 | `internal/skill/activity.go` | 活动 Skill 快照、参数展开、模型冲突与白名单交集 |
| 新建 | `internal/skill/profile.go` | 请求级模型、工具、只读根和隔离深度配置 |
| 新建 | `internal/skill/history.go` | 最近 N 个完整用户轮次提取 |
| 新建 | `internal/skill/prompt.go` | 轻量目录与活动 SOP 系统区块文本 |
| 新建 | `internal/skill/builtin.go` | 内置 Skill 嵌入来源和辅助资源物化 |
| 新建 | `internal/skill/builtins/commit/SKILL.md` | 共享提交工作流样板 |
| 新建 | `internal/skill/builtins/review/SKILL.md` | 独立审查工作流样板 |
| 新建 | `internal/skill/builtins/test/SKILL.md` | 独立测试工作流样板 |
| 新建 | `internal/skill/parser_test.go` | 格式、字段、正文和边界测试 |
| 新建 | `internal/skill/discovery_test.go` | 两种包形态、路径与超限测试 |
| 新建 | `internal/skill/manager_test.go` | 覆盖、冲突、启动校验与热更新测试 |
| 新建 | `internal/skill/activity_test.go` | 模板、排序、模型冲突和原子性测试 |
| 新建 | `internal/skill/profile_test.go` | 工具交集、Plan 限制和系统工具测试 |
| 新建 | `internal/skill/history_test.go` | 完整用户轮次提取测试 |
| 新建 | `internal/tool/load_skill.go` | `load_skill` 系统工具声明和参数 schema |
| 新建 | `internal/tool/view.go` | Registry 不可变过滤视图 |
| 新建 | `internal/tool/read_scope.go` | 请求级项目根和额外只读根 Context |
| 新建 | `internal/tool/view_test.go` | 工具过滤、顺序和系统工具保留测试 |
| 新建 | `internal/tool/read_scope_test.go` | 多根读取和路径越界测试 |
| 修改 | `internal/tool/registry.go` | 工具名称枚举与过滤视图支持 |
| 修改 | `internal/tool/path.go` | 多只读根真实路径解析与无符号链接打开 |
| 修改 | `internal/tool/read.go` | 使用请求级 ReadScope |
| 修改 | `internal/tool/glob.go` | 在项目根与活动包根中确定性查找 |
| 修改 | `internal/tool/grep.go` | 在允许只读根中搜索并返回可定位路径 |
| 修改 | `internal/provider/provider.go` | `ChatRequest.Model` 请求级模型覆盖字段 |
| 修改 | `internal/provider/anthropic.go` | Anthropic 请求采用覆盖模型 |
| 修改 | `internal/provider/openai.go` | OpenAI 请求采用覆盖模型 |
| 修改 | `internal/provider/anthropic_schema_test.go` | Anthropic 模型覆盖序列化测试 |
| 修改 | `internal/provider/tool_parse_test.go` | OpenAI 模型覆盖与工具定义测试 |
| 修改 | `internal/prompt/section.go` | BuildRequest 接受 Skill 目录和活动区块 |
| 修改 | `internal/prompt/builder.go` | 稳定目录区块组装 |
| 修改 | `internal/prompt/dynamic.go` | 高显著度活动 Skill 区块排序 |
| 修改 | `internal/prompt/prompt_test.go` | 两阶段加载、区块位置和轮次稳定性测试 |
| 新建 | `internal/orchestrator/skill_runtime.go` | Skill 准备入口与系统工具处理 |
| 新建 | `internal/orchestrator/independent.go` | 独立临时会话执行与摘要回流结果 |
| 新建 | `internal/orchestrator/execution_state.go` | Activity、Profile、持久化、Memory 和深度状态 |
| 新建 | `internal/orchestrator/skill_runtime_test.go` | 共享加载与 Profile 切换测试 |
| 新建 | `internal/orchestrator/independent_test.go` | 隔离、权限事件、取消和递归测试 |
| 修改 | `internal/orchestrator/run_request.go` | RunRequest 携带 ExecutionProfile，新增 RunResult |
| 修改 | `internal/orchestrator/agent_loop.go` | 返回最终结果并支持 iteration 边界切换 Profile |
| 修改 | `internal/orchestrator/chat.go` | Skill Prompt、模型、工具视图与 ReadScope 接入 |
| 修改 | `internal/orchestrator/tool_batches.go` | 系统工具分支及请求视图二次校验 |
| 修改 | `internal/orchestrator/tool_definitions.go` | 从请求级 Registry View 生成定义 |
| 修改 | `internal/events/events.go` | 独立运行和 transient 事件标记 |
| 修改 | `internal/events/events_test.go` | 新事件字段兼容性测试 |
| 修改 | `internal/command/definition.go` | Definition 和 Suggestion 增加 Badge |
| 修改 | `internal/command/controller.go` | 增加统一 `ExecuteSkill` 控制接口 |
| 修改 | `internal/command/builtins.go` | 移除硬编码 review，保留隐藏 `/rv` 转发 |
| 修改 | `internal/command/completion.go` | 补全候选携带 Badge |
| 修改 | `internal/command/builtins_test.go` | help、review 迁移和 `/rv` 兼容测试 |
| 修改 | `internal/command/completion_test.go` | Skill Badge 和隐藏命令补全测试 |
| 修改 | `internal/tui/command_menu.go` | 补全菜单显示 Skill 模式标记 |
| 修改 | `internal/tui/messages.go` | transient 工具轨迹独立展示和清理 |
| 修改 | `internal/tui/status.go` | 当前 Skill 与实际请求模型状态 |
| 修改 | `internal/tui/command_menu_test.go` | Badge 渲染测试 |
| 修改 | `internal/tui/messages_test.go` | transient 轨迹不持久显示测试 |
| 新建 | `internal/app/skills.go` | 热更新、动态命令重建和 Skill 调用入口 |
| 新建 | `internal/app/skills_test.go` | App 级共享、独立、热更新和生命周期测试 |
| 修改 | `internal/app/deps.go` | 注入 Skill Manager |
| 修改 | `internal/app/app.go` | 主 Activity、generation 和基础命令定义 |
| 修改 | `internal/app/commands.go` | Enter、Tab、普通提交前刷新 Skill |
| 修改 | `internal/app/command_controller.go` | `ExecuteSkill` 与 `/clear` 清活动状态 |
| 修改 | `internal/app/update.go` | 独立事件、请求模型和 transient 生命周期 |
| 修改 | `internal/app/commands_test.go` | 动态 help、补全与输入分流回归测试 |
| 修改 | `internal/app/update_test.go` | 独立事件和状态恢复测试 |
| 修改 | `cmd/xagent/main.go` | 注册系统工具并按三级来源装配 Skill Manager |
| 新建 | `cmd/xagent/main_test.go` | 启动路径、工具校验和保留命令装配测试 |
| 修改 | `README.md` | Skill 定义、目录、模式、样板和热更新说明 |

## 领域核心

### T1：定义 Skill 领域类型和固定限制

**文件：** `internal/skill/types.go`  
**依赖：** 无

**步骤：**

1. 定义 `Source`、`Mode`、`Metadata`、`Definition`、`CatalogItem`、`Snapshot`、`Invocation` 和 `PreparedInvocation`。
2. 加入名称、文件数量、入口大小、展开正文和参数大小常量。
3. 为 Snapshot 的映射和切片提供复制函数，禁止调用方修改已发布状态。

**验证：** 运行 `go test ./internal/skill`，期望新包可编译且无循环依赖。

### T2：解析 frontmatter 边界与 Markdown 正文

**文件：** `internal/skill/parser.go`  
**依赖：** T1

**步骤：**

1. 只接受文件开头的 `---` 和第二个独立 `---` 之间的 YAML。
2. 使用已有 YAML 库的严格字段模式解码 Metadata。
3. 保留 Markdown 正文原始内容，仅对空正文做拒绝判断。

**验证：** 运行 `go test ./internal/skill -run TestParseFrontmatterBoundary`，期望合法输入解析成功，缺少边界的输入失败。

### T3：校验 Skill 元数据

**文件：** `internal/skill/parser.go`  
**依赖：** T2

**步骤：**

1. 将名称规范化为小写，并校验 `[a-z0-9][a-z0-9_-]{0,63}`、非空说明和合法 mode。
2. 拒绝负 history，并拒绝 shared mode 携带非零 history。
3. 规范化 `allowed_tools`：去空白、拒绝空项、精确保留大小写、去重并排序。

**验证：** 运行 `go test ./internal/skill -run TestValidateMetadata`，期望非法名称、未知字段、非法 mode/history 和空工具项均失败。

### T4：覆盖解析器和模板边界测试

**文件：** `internal/skill/parser_test.go`  
**依赖：** T3

**步骤：**

1. 增加合法单文件、合法目录入口、未知 YAML 字段、空正文和大小写名称用例。
2. 增加入口 256 KiB、正文 128 KiB 和名称 64 字符边界用例。
3. 断言错误文本不回显正文中的模拟密钥。

**验证：** 运行 `go test ./internal/skill -run 'TestParse|TestValidate'`，期望所有解析和边界用例通过。

### T5：发现根目录单文件 Skill

**文件：** `internal/skill/discovery.go`  
**依赖：** T3

**步骤：**

1. 扫描来源根直接包含的 `*.md`，按规范路径排序后读取。
2. 记录 Source、EntryPath、PackageRoot 和内容指纹。
3. 把单文件读取或解析错误转换为脱敏 Diagnostic，并继续扫描。

**验证：** 运行 `go test ./internal/skill -run TestDiscoverSingleFiles`，期望损坏文件被跳过且其他文件仍返回。

### T6：发现目录型 Skill 并限制路径

**文件：** `internal/skill/discovery.go`  
**依赖：** T5

**步骤：**

1. 仅识别来源根直接子目录里的 `SKILL.md`，不把辅助 Markdown 当入口。
2. 绝对化并求值来源根和包根，拒绝入口符号链接及越出来源根的路径。
3. 超过 256 个入口或入口大小限制时生成诊断，输出保持确定顺序。

**验证：** 运行 `go test ./internal/skill -run TestDiscoverDirectoryPackages`，期望辅助文件不进入目录且符号链接入口被拒绝。

### T7：覆盖发现与诊断测试

**文件：** `internal/skill/discovery_test.go`  
**依赖：** T6

**步骤：**

1. 用临时目录构造单文件、目录型、损坏、高优先级损坏和辅助文件场景。
2. 覆盖入口数量、文件大小、符号链接和路径脱敏行为。
3. 重排文件创建顺序并断言发现结果完全相同。

**验证：** 运行 `go test ./internal/skill -run TestDiscover`，期望全部发现测试通过。

### T8：构建三级有效快照

**文件：** `internal/skill/snapshot.go`  
**依赖：** T7

**步骤：**

1. 按 builtin、user、project 的优先级合并有效 Definition。
2. 只让有效高层定义覆盖低层定义；无效入口不参与遮蔽。
3. 同一来源存在两个有效规范同名定义时返回包含双方来源的快照级错误。

**验证：** 运行 `go test ./internal/skill -run TestBuildSnapshotPrecedence`，期望项目覆盖用户、用户覆盖内置，同层冲突失败。

### T9：校验工具白名单和动态命令资格

**文件：** `internal/skill/snapshot.go`  
**依赖：** T8

**步骤：**

1. 对最终生效定义校验大小写精确的工具名称；任一缺失工具使候选快照失败。
2. 把基础命令及别名、`load_skill` 纳入保留名称集合。
3. 冲突 Skill 保留在 Definitions 和 Catalog 中，但设置 `SlashEnabled=false` 并生成诊断。

**验证：** 运行 `go test ./internal/skill -run TestSnapshotValidation`，期望未知工具失败、命令冲突仅禁用短命令。

### T10：覆盖快照构建测试

**文件：** `internal/skill/manager_test.go`  
**依赖：** T9

**步骤：**

1. 覆盖三级覆盖、同层大小写重名、未知工具和保留命令冲突。
2. 断言 Catalog、Definitions 和 Diagnostics 稳定排序。
3. 断言返回快照被调用方修改后不影响内部状态。

**验证：** 运行 `go test ./internal/skill -run 'TestBuildSnapshot|TestSnapshot'`，期望全部快照测试通过。

### T11：实现 Manager 严格首次加载

**文件：** `internal/skill/manager.go`  
**依赖：** T10

**步骤：**

1. 定义 `Limits`、`SourceFS`、`ManagerOptions`、`RefreshResult` 和 `Manager`。
2. `NewManager` 扫描全部来源、构造 generation=1 的有效快照并保存清单指纹。
3. 首次同层冲突或白名单错误直接返回错误；单文件错误只进入诊断。

**验证：** 运行 `go test ./internal/skill -run TestManagerInitialLoad`，期望合法来源启动成功，快照级错误阻止启动。

### T12：实现候选快照原子热更新

**文件：** `internal/skill/manager.go`  
**依赖：** T11

**步骤：**

1. `RefreshIfChanged` 先比较轻量文件清单指纹，无变化时不重新解析正文。
2. 有变化时在锁内构造完整候选，通过全部校验后一次性替换快照并递增 generation。
3. 候选失败时保留旧快照和 generation，返回脱敏诊断；`Resolve` 始终返回 Definition 副本。

**验证：** 运行 `go test ./internal/skill -run TestManagerRefresh`，期望成功刷新换代，冲突和未知工具刷新保留旧快照。

### T13：覆盖 Manager 并发和热更新测试

**文件：** `internal/skill/manager_test.go`  
**依赖：** T12

**步骤：**

1. 覆盖新增、修改、删除、解析错误、同层冲突和未知工具刷新。
2. 断言无文件变化时 generation 与解析计数不变。
3. 并发调用 Snapshot、Resolve 和 RefreshIfChanged，断言不会观察到混合 generation。

**验证：** 运行 `go test -race ./internal/skill -run TestManager`，期望 Manager 测试无数据竞争。

### T14：实现参数字面展开和单项激活

**文件：** `internal/skill/activity.go`  
**依赖：** T4

**步骤：**

1. 定义 `Activated`、`Activity` 和 `ActivitySnapshot`。
2. 校验参数不超过 32 KiB，用完整参数对所有 `{{args}}` 做一次 `strings.ReplaceAll`。
3. 保存展开后的完整 SOP、模型、工具、来源、包根和 Definition 指纹副本。

**验证：** 运行 `go test ./internal/skill -run TestActivityTemplateExpansion`，期望参数内的 `{{args}}` 不被二次展开。

### T15：实现多 Skill 原子活动状态

**文件：** `internal/skill/activity.go`  
**依赖：** T14

**步骤：**

1. 激活前在候选副本中校验非空模型一致性，冲突时不改变原状态。
2. 按规范名称替换同名 Skill，输出时稳定排序，并计算非空白名单的交集。
3. 实现 `Clear`，恢复空模型、无限制白名单和空只读根。

**验证：** 运行 `go test ./internal/skill -run TestActivityAtomicActivation`，期望重复激活替换、模型冲突回滚、Clear 恢复默认。

### T16：覆盖 Activity 行为测试

**文件：** `internal/skill/activity_test.go`  
**依赖：** T15

**步骤：**

1. 覆盖多 Skill 排序、重复激活、白名单交集、空白名单和模型一致性。
2. 激活后修改原 Definition，断言 Activity 中的 SOP 与元信息不变。
3. 让失败激活发生在各校验分支，断言前后 Snapshot 完全相同。

**验证：** 运行 `go test -race ./internal/skill -run TestActivity`，期望所有活动状态测试通过且无数据竞争。

### T17：构建请求级 ExecutionProfile

**文件：** `internal/skill/profile.go`  
**依赖：** T16

**步骤：**

1. 定义 `ExecutionProfile` 和 `ProfileRequest`，复制 Catalog、Activity、模型和只读根。
2. 将活动白名单与基础工具集合求交；Plan 只读模式继续只保留 Read、Glob、Grep。
3. 无条件加入已注册的 `load_skill`，并携带 IndependentDepth、Persist 和 UpdateMemory。

**验证：** 运行 `go test ./internal/skill -run TestBuildProfile`，期望白名单只会收窄工具且系统工具始终存在。

### T18：覆盖 Profile 组合测试

**文件：** `internal/skill/profile_test.go`  
**依赖：** T17

**步骤：**

1. 覆盖无限制、单白名单、多白名单交集和空交集。
2. 覆盖 Plan Mode 与 Skill 白名单交集、未知基础工具和 `load_skill` 保留。
3. 断言模型、只读根、持久化和独立深度没有跨 Profile 污染。

**验证：** 运行 `go test ./internal/skill -run TestBuildProfile`，期望全部组合用例通过。

### T19：提取最近 N 个完整用户轮次

**文件：** `internal/skill/history.go`  
**依赖：** T1

**步骤：**

1. 从 `conversation.ContextMessages` 中按用户消息划分完整轮次。
2. 只选择已结束轮次，并保留该轮的 assistant、tool_call、tool_result 与上下文边界顺序。
3. 返回最后 N 轮的深拷贝；N=0 返回空，不修改原 Conversation。

**验证：** 运行 `go test ./internal/skill -run TestRecentCompleteTurns`，期望工具链不被截断且未完成尾轮不被选入。

### T20：覆盖历史轮次边界测试

**文件：** `internal/skill/history_test.go`  
**依赖：** T19

**步骤：**

1. 构造普通轮次、含多个工具调用的轮次、尾部未完成轮次和压缩边界消息。
2. 覆盖 N=0、N 大于现有轮次和 nil Conversation。
3. 修改返回消息并断言主 Conversation 未变化。

**验证：** 运行 `go test ./internal/skill -run TestRecentCompleteTurns`，期望所有轮次边界测试通过。

### T21：生成两阶段 Skill Prompt 文本

**文件：** `internal/skill/prompt.go`  
**依赖：** T10、T16

**步骤：**

1. Catalog 文本只输出规范名称、说明和可用 `load_skill` 的提示，不包含正文或资源内容。
2. Active 文本输出明确系统边界、名称、来源、包根和展开后的完整 SOP。
3. 两类输出均按规范名称排序，并对动态路径和标签字符做安全转义。

**验证：** 运行 `go test ./internal/skill -run TestSkillPromptText`，期望未激活正文不出现在 Catalog，活动 SOP 完整出现。

### T22：覆盖 Skill Prompt 快照测试

**文件：** `internal/skill/activity_test.go`  
**依赖：** T21

**步骤：**

1. 断言不同发现顺序生成相同 Catalog 文本。
2. 断言不同激活顺序生成相同 Active 文本。
3. 在说明、路径和参数中放入标签与模拟密钥，断言输出经过边界转义和脱敏函数。

**验证：** 运行 `go test ./internal/skill -run TestSkillPromptText`，期望确定性和脱敏断言通过。

### T23：添加三个内置目录型 Skill

**文件：** `internal/skill/builtins/commit/SKILL.md`、`internal/skill/builtins/review/SKILL.md`、`internal/skill/builtins/test/SKILL.md`  
**依赖：** T3

**步骤：**

1. commit 使用 shared、history=0、默认模型和 `Read/Glob/Grep/Bash`，正文覆盖检查改动、验证和创建提交。
2. review 使用 isolated、history=1、默认模型和同一工具白名单，正文要求只审查不修改并按严重程度输出。
3. test 使用 isolated、history=1、默认模型和同一工具白名单，正文要求发现测试入口、运行测试并报告实际证据。

**验证：** 运行 `go test ./internal/skill -run TestBuiltinMetadata`，期望三个入口均可由同一 Parser 解析。

### T24：嵌入并物化内置能力包

**文件：** `internal/skill/builtin.go`、`internal/skill/manager_test.go`  
**依赖：** T23

**步骤：**

1. 用 `go:embed` 暴露 builtins 来源，仍通过 Discovery 和 Parser 构造 Definition。
2. 仅在激活需要文件路径时将目标包物化到进程临时目录，限制目录权限并返回规范包根。
3. 提供清理函数；物化失败时返回激活错误且不更新 Activity。

**验证：** 运行 `go test ./internal/skill -run TestBuiltin`，期望三个样板可发现、元数据正确且临时资源可清理。

## 工具、Provider 与 Prompt 集成

### T25：声明系统级 `load_skill` 工具

**文件：** `internal/tool/load_skill.go`  
**依赖：** 无

**步骤：**

1. 定义固定工具名称、说明以及必填 `name`、可选 `args` schema。
2. 实现 sentinel Tool，使其可注册和生成 Provider 定义，但普通 Execute 返回明确的内部路由错误。
3. 将风险级别设为安全；实际加载仍由 Orchestrator 拦截。

**验证：** 运行 `go test ./internal/tool -run TestLoadSkillTool`，期望 schema 稳定且直接执行不会加载文件。

### T26：实现 Registry 不可变过滤视图

**文件：** `internal/tool/registry.go`、`internal/tool/view.go`  
**依赖：** T25

**步骤：**

1. 增加稳定的工具名称枚举，并让 Registry View 复用底层 Tool 实例。
2. 按 `AllowedNames`、`ReadOnly` 过滤原始顺序，再追加 `AlwaysInclude` 中真实存在的工具。
3. 过滤失败返回错误，不修改基础 Registry。

**验证：** 运行 `go test ./internal/tool -run TestRegistryView`，期望视图顺序稳定、基础 Registry 不变。

### T27：覆盖工具视图和系统工具测试

**文件：** `internal/tool/view_test.go`  
**依赖：** T26

**步骤：**

1. 覆盖 nil 白名单、空集合、部分集合和只读模式。
2. 断言 `load_skill` 在极窄或空白名单下仍出现。
3. 断言视图 Get、List、AnthropicDefinitions 和 OpenAIDefinitions 返回同一工具集合。

**验证：** 运行 `go test ./internal/tool -run 'TestRegistryView|TestLoadSkillTool'`，期望视图与定义完全一致。

### T28：实现请求级 ReadScope 和多根路径解析

**文件：** `internal/tool/read_scope.go`、`internal/tool/path.go`  
**依赖：** 无

**步骤：**

1. 用 Context 传递 `ReadScope{ProjectRoot, ExtraRoots}`，对所有根绝对化、求值并去重。
2. 新增只读路径解析：相对路径优先项目根，绝对路径必须属于项目根或额外根。
3. 打开文件和遍历前再次求值现有祖先并使用 no-follow 语义，拒绝包根外的符号链接。

**验证：** 运行 `go test ./internal/tool -run TestReadScopeResolution`，期望允许包内读取并拒绝路径穿越和 symlink 越界。

### T29：让 Read 使用请求级只读根

**文件：** `internal/tool/read.go`、`internal/tool/path.go`  
**依赖：** T28

**步骤：**

1. Read 从 Context 取 ReadScope；未设置时保持现有项目根行为。
2. 对额外根文件返回稳定的包内显示路径，不暴露不必要的用户目录前缀。
3. 保持 WriteProjectFile 与写工具只使用项目根。

**验证：** 运行 `go test ./internal/tool -run TestReadToolExtraRoot`，期望 Read 可读包内文件，Write 仍拒绝项目外目标。

### T30：让 Glob 和 Grep 使用请求级只读根

**文件：** `internal/tool/glob.go`、`internal/tool/grep.go`  
**依赖：** T29

**步骤：**

1. Glob 在所有允许根中匹配，去重并稳定排序，继续限制最多 200 个结果。
2. Grep 只遍历解析后的单个允许根或路径，读取文件时复用多根安全打开逻辑。
3. 结果路径标注所属项目或 Skill 包，避免不同根同名文件混淆。

**验证：** 运行 `go test ./internal/tool -run 'TestGlobToolExtraRoot|TestGrepToolExtraRoot'`，期望额外根可搜索且项目外路径被拒绝。

### T31：覆盖只读根安全与写入隔离测试

**文件：** `internal/tool/read_scope_test.go`  
**依赖：** T30

**步骤：**

1. 覆盖 Read、Glob、Grep 对项目根、两个额外根和绝对路径的访问。
2. 构造指向包根外的文件和目录 symlink，断言全部读取类工具拒绝。
3. 调用 Write/Edit 指向额外根，断言仍按项目根越界错误处理。

**验证：** 运行 `go test ./internal/tool -run 'TestReadScope|Test.*ExtraRoot'`，期望多根访问与写入隔离测试通过。

### T32：增加 Provider 请求级模型字段

**文件：** `internal/provider/provider.go`、`internal/provider/anthropic.go`、`internal/provider/openai.go`  
**依赖：** 无

**步骤：**

1. 在 `ChatRequest` 增加可选 `Model`。
2. Anthropic 与 OpenAI 构造请求时优先使用非空覆盖值，否则使用 Provider 配置默认模型。
3. 不修改共享 Provider 配置、协议、Key、Base URL、Thinking 或超时。

**验证：** 运行 `go test ./internal/provider -run TestRequestModelOverride`，期望覆盖和默认两条路径均通过。

### T33：覆盖 Provider 模型隔离测试

**文件：** `internal/provider/anthropic_schema_test.go`、`internal/provider/tool_parse_test.go`  
**依赖：** T32

**步骤：**

1. 捕获 Anthropic 请求并断言 `ChatRequest.Model` 覆盖序列化模型。
2. 捕获 OpenAI 请求并断言覆盖模型和工具定义同时保留。
3. 连续发送覆盖请求和默认请求，断言第二次恢复配置模型。

**验证：** 运行 `go test ./internal/provider -run 'Test.*Model'`，期望模型不会跨请求污染。

### T34：扩展 Prompt BuildRequest

**文件：** `internal/prompt/section.go`  
**依赖：** T21

**步骤：**

1. 为 BuildRequest 增加 Skill Catalog 和 Active Skill 内容字段。
2. 保持调用方未提供新字段时输出与现有行为等价。
3. 明确两个区块名称分别为 `skill-catalog` 和 `active-skills`。

**验证：** 运行 `go test ./internal/prompt -run TestBuildWithoutSkills`，期望无 Skill 时现有 Prompt 快照不变。

### T35：组装稳定目录和高显著度活动区块

**文件：** `internal/prompt/builder.go`、`internal/prompt/dynamic.go`  
**依赖：** T34

**步骤：**

1. 非空 Catalog 作为 stable block 加入固定系统区块之后。
2. 非空 Active Skills 作为独立 dynamic block，排在普通 runtime reminder 之前。
3. 每次 Build 都从传入 Profile 重建活动区块，不写入 Conversation。

**验证：** 运行 `go test ./internal/prompt -run TestSkillBlockOrdering`，期望区块稳定性、位置和空值行为正确。

### T36：覆盖两阶段 Prompt 集成测试

**文件：** `internal/prompt/prompt_test.go`  
**依赖：** T35

**步骤：**

1. 未激活时断言请求只含目录名称和说明，不含 SOP 或辅助资源文本。
2. 激活后在 iteration 1、2、3 构建 Prompt，断言相同 SOP 每轮存在。
3. 断言固定安全区块先于 active-skills，active-skills 先于 runtime reminder。

**验证：** 运行 `go test ./internal/prompt -run 'TestSkill|TestBuildWithoutSkills'`，期望两阶段与排序测试通过。

## 命令、事件与 TUI

### T37：让命令元数据携带 Skill Badge

**文件：** `internal/command/definition.go`、`internal/command/completion.go`、`internal/command/builtins.go`  
**依赖：** 无

**步骤：**

1. 在 Definition 和 Suggestion 增加 Badge，并在补全中复制该字段。
2. `/help` 在 Badge 非空时显示 `Skill/shared` 或 `Skill/isolated`。
3. 不改变固定命令的帮助排序、别名解析和隐藏规则。

**验证：** 运行 `go test ./internal/command -run 'TestHelp|TestComplete'`，期望 Badge 可见且旧命令输出保持兼容。

### T38：迁移 review 命令并保留 `/rv` shim

**文件：** `internal/command/controller.go`、`internal/command/builtins.go`  
**依赖：** T37

**步骤：**

1. Controller 增加 `ExecuteSkill(name, args, raw)`。
2. 删除硬编码 `ReviewPrompt` 和可见 review Definition。
3. 注册隐藏 `/rv` 兼容 Definition，固定调用 `ExecuteSkill("review", args, raw)`。

**验证：** 运行 `go test ./internal/command -run TestReviewMigration`，期望 `/review` 不再由固定命令占用，`/rv` 仍可转发且不参与补全。

### T39：覆盖动态命令元数据回归测试

**文件：** `internal/command/builtins_test.go`、`internal/command/completion_test.go`  
**依赖：** T38

**步骤：**

1. 更新 Controller fake 实现并记录 ExecuteSkill 参数。
2. 断言 help 显示 Badge、隐藏 `/rv` 不显示、补全候选保留 Badge。
3. 断言全部基础命令及别名仍通过 Registry 冲突检测。

**验证：** 运行 `go test ./internal/command`，期望命令包全部测试通过。

### T40：标记独立执行 transient 事件

**文件：** `internal/events/events.go`、`internal/events/events_test.go`  
**依赖：** 无

**步骤：**

1. 在 Event 增加 `Transient` 和独立执行标识，不改变现有 Type 常量。
2. 规定独立流式文本、思考和工具进度为 transient，最终摘要和主用户提交不是 transient。
3. 覆盖零值事件与现有事件构造的兼容性。

**验证：** 运行 `go test ./internal/events`，期望旧事件零值行为不变且新标记可区分。

### T41：隔离 MessagesView 的 transient 轨迹

**文件：** `internal/tui/messages.go`  
**依赖：** T40

**步骤：**

1. 为 MessagesView 增加独立临时缓冲区，不把 transient 消息追加到持久 messages。
2. transient 工具状态按 CallID 更新并实时渲染。
3. 独立执行结束、失败或取消时清空临时缓冲，仅由主事件提交最终摘要。

**验证：** 运行 `go test ./internal/tui -run TestTransientMessages`，期望运行中可见、收尾后工具轨迹消失。

### T42：显示 Skill Badge 和实际请求模型

**文件：** `internal/tui/command_menu.go`、`internal/tui/status.go`  
**依赖：** T37

**步骤：**

1. CommandMenuItem 增加 Badge，并在候选名称旁显示模式标记。
2. Status 增加当前活动 Skill 摘要和临时实际模型字段。
3. 请求结束后清空临时模型展示，继续显示应用默认模型。

**验证：** 运行 `go test ./internal/tui -run 'TestCommandMenuBadge|TestStatusSkill'`，期望菜单和状态文本可观测且默认状态不变。

### T43：覆盖 transient、Badge 与状态测试

**文件：** `internal/tui/command_menu_test.go`、`internal/tui/messages_test.go`、`internal/tui/status_test.go`  
**依赖：** T41、T42

**步骤：**

1. 断言多匹配菜单显示 `Skill/shared` 与 `Skill/isolated`。
2. 断言 transient 文本、思考和工具进度实时出现，完成清理后不进入历史渲染。
3. 断言覆盖模型仅在请求期间显示，完成后恢复默认模型。

**验证：** 运行 `go test ./internal/tui`，期望 TUI 包全部测试通过。

## Orchestrator 执行协调

### T44：引入 RunResult 和请求执行状态

**文件：** `internal/orchestrator/run_request.go`、`internal/orchestrator/execution_state.go`  
**依赖：** T17

**步骤：**

1. 为 RunRequest 增加 ExecutionProfile，定义包含最终文本、Usage、Duration 和 StopReason 的 RunResult。
2. 定义请求状态，持有当前 Activity/Profile、持久化开关、Memory 开关和独立深度。
3. 没有 Profile 的旧调用自动构造等价默认状态。

**验证：** 运行 `go test ./internal/orchestrator -run TestDefaultExecutionState`，期望旧 Send 路径仍使用默认模型和完整工具集。

### T45：让 Agent Loop 返回最终结果

**文件：** `internal/orchestrator/agent_loop.go`、`internal/orchestrator/stream_collector.go`  
**依赖：** T44

**步骤：**

1. 将 runAgentLoop 改为返回 RunResult，同时保持既有事件顺序。
2. 累计每轮 Usage，并记录最后完整 assistant 文本和实际 StopReason。
3. 根据执行状态决定是否 Save 和更新 Memory；默认状态保持现有行为。

**验证：** 运行 `go test ./internal/orchestrator -run 'TestAgentLoop|TestDefaultExecutionState'`，期望默认事件、保存和 Memory 测试不回退。

### T46：按 Profile 构建 Provider 请求

**文件：** `internal/orchestrator/chat.go`、`internal/orchestrator/tool_definitions.go`  
**依赖：** T27、T33、T36、T45

**步骤：**

1. 从 Profile 填入 Skill Catalog、Active Skills、模型覆盖和 ReadScope。
2. 从 Profile 的工具名构建 Registry View，并由该视图生成 Provider Tool Definitions。
3. iteration 边界重新读取当前 Profile，使共享 Skill 加载后下一轮生效。

**验证：** 运行 `go test ./internal/orchestrator -run TestProfileChatRequest`，期望模型、Prompt、工具和只读根来自同一 Profile。

### T47：使用请求工具视图做执行层二次校验

**文件：** `internal/orchestrator/tool_batches.go`  
**依赖：** T46

**步骤：**

1. makeToolBatches、风险分类和 filterToolCall 都接收当前 Registry View。
2. 对 Provider 伪造的未暴露工具返回现有可恢复未知/拒绝结果，不交给 Executor。
3. 通过视图的工具继续走 Authorizer、确认和现有 Executor。

**验证：** 运行 `go test ./internal/orchestrator -run TestFilteredToolCannotExecute`，期望伪造调用被拦截、允许的危险工具仍请求确认。

### T48：统一准备 Skill 调用

**文件：** `internal/orchestrator/skill_runtime.go`  
**依赖：** T12、T16、T24

**步骤：**

1. 定义 SkillRuntime 接口和基于 Manager 的实现。
2. 规范化名称、解析当前快照、展开参数并在候选 Activity 中准备激活。
3. shared 和 isolated 都返回 PreparedInvocation；任何错误不修改调用方 Activity。

**验证：** 运行 `go test ./internal/orchestrator -run TestPrepareSkill`，期望短命令和 Agent 工具入口得到相同准备结果。

### T49：处理 Agent 的共享 `load_skill`

**文件：** `internal/orchestrator/skill_runtime.go`、`internal/orchestrator/tool_batches.go`、`internal/orchestrator/agent_loop.go`  
**依赖：** T47、T48

**步骤：**

1. 在普通工具分批前识别 `load_skill` 并严格解析 name/args。
2. 多个加载调用按 Provider 返回顺序串行处理，并生成正常 tool_call/tool_result 历史。
3. shared 激活成功后在 iteration 结束处原子替换 Activity/Profile，下一轮使用新 SOP、模型和工具视图。

**验证：** 运行 `go test ./internal/orchestrator -run TestLoadSharedSkillAtIterationBoundary`，期望首轮只有目录，次轮出现 SOP 和收窄工具。

### T50：覆盖共享加载和冲突测试

**文件：** `internal/orchestrator/skill_runtime_test.go`  
**依赖：** T49

**步骤：**

1. 用 fake Provider 连续返回加载工具调用和最终文本，捕获每轮请求。
2. 断言活动 SOP、模型和白名单从下一 iteration 生效并持续存在。
3. 覆盖模型冲突、未知 Skill、超限参数和多个串行加载，断言失败不改变旧 Profile。

**验证：** 运行 `go test ./internal/orchestrator -run 'TestLoadShared|TestPrepareSkill'`，期望共享加载测试全部通过。

### T51：构造独立临时 Conversation

**文件：** `internal/orchestrator/independent.go`  
**依赖：** T19、T48

**步骤：**

1. 从主 Conversation 复制目标 Skill 配置的最近 N 个完整轮次。
2. 在临时 Conversation 末尾追加原始 Skill 调用，创建只激活目标 Skill 的临时 Activity。
3. 不复制主 Activity、Conversation Context 指针或可变消息切片。

**验证：** 运行 `go test ./internal/orchestrator -run TestBuildIndependentConversation`，期望历史完整且主状态修改前后相同。

### T52：运行不持久化、不更新 Memory 的独立 Agent

**文件：** `internal/orchestrator/independent.go`、`internal/orchestrator/execution_state.go`  
**依赖：** T45、T51

**步骤：**

1. 构建 Persist=false、UpdateMemory=false、IndependentDepth=1 的临时 Profile。
2. 复用 Provider、Executor、Authorizer、取消 Context、请求超时和事件通道调用 Agent Loop。
3. 返回独立 RunResult.FinalText，不调用第二个摘要模型，也不创建 Store 会话。

**验证：** 运行 `go test ./internal/orchestrator -run TestRunIndependentNoSideEffects`，期望 Provider 只完成独立调用，Store 和 Memory 均无写入。

### T53：转发独立事件并阻止递归

**文件：** `internal/orchestrator/independent.go`、`internal/orchestrator/skill_runtime.go`、`internal/orchestrator/tool_batches.go`  
**依赖：** T40、T52

**步骤：**

1. 将独立执行的文本、思考、工具与进度事件标为 transient 后转发，权限确认通道保持原对象。
2. depth>0 时再次加载 isolated Skill 返回明确可恢复工具错误。
3. depth>0 时加载 shared Skill 只更新临时 Activity，不接触主 Activity。

**验证：** 运行 `go test ./internal/orchestrator -run TestIndependentNestedSkillRules`，期望隔离递归被拒绝，共享加载只影响子上下文。

### T54：处理主 Agent 触发的 isolated Skill

**文件：** `internal/orchestrator/agent_loop.go`、`internal/orchestrator/skill_runtime.go`  
**依赖：** T53

**步骤：**

1. 主循环识别 isolated PreparedInvocation 后，不把系统加载工具调用写入主 Conversation。
2. 暂停父循环并执行 Independent Runner；成功时将最终文本作为当前主轮次唯一 Assistant 回复。
3. 成功后结束父循环；失败、取消或超时时不写成功摘要，也不再次调用主模型。

**验证：** 运行 `go test ./internal/orchestrator -run TestAgentTriggeredIsolatedSkill`，期望主历史只有用户请求和最终摘要，Provider 调用次数无额外总结轮次。

### T55：覆盖独立执行、权限和取消测试

**文件：** `internal/orchestrator/independent_test.go`  
**依赖：** T54

**步骤：**

1. 覆盖短命令独立运行和 Agent 主动 isolated 加载两条入口。
2. 覆盖危险工具确认、实时 transient 事件、取消、超时、Provider 失败和空最终文本。
3. 断言临时工具轨迹、思考、Activity、Store 和 Memory 均不污染主状态。

**验证：** 运行 `go test -race ./internal/orchestrator -run 'TestIndependent|TestAgentTriggeredIsolated'`，期望隔离测试通过且无数据竞争。

## App、启动装配与文档

### T56：注入 Skill Manager 并维护主 Activity

**文件：** `internal/app/deps.go`、`internal/app/app.go`  
**依赖：** T12、T16

**步骤：**

1. Deps 增加 Skill Manager，Model 增加主 Activity、当前 generation 和基础命令 Definition 副本。
2. New 初始化空 Activity，并把 SkillRuntime 注入 Orchestrator。
3. startNewConversation、loadConversation 和 resetCommandState 都创建新的空 Activity。

**验证：** 运行 `go test ./internal/app -run TestSkillActivityLifecycle`，期望新建和切换会话不继承活动 Skill。

### T57：在交互边界原子刷新动态命令

**文件：** `internal/app/skills.go`、`internal/app/commands.go`、`internal/app/app.go`  
**依赖：** T39、T56

**步骤：**

1. 在 Enter、Tab 和提交普通请求前调用 Manager.RefreshIfChanged。
2. generation 改变时从基础 Definitions 和 `SlashEnabled` Catalog 重建新 Registry，再一次性替换 Model 引用。
3. 每个动态 Handler 只调用 Controller.ExecuteSkill；刷新失败显示诊断并保留旧 Registry/generation。

**验证：** 运行 `go test ./internal/app -run TestSkillCommandRefresh`，期望新增、修改、删除后命令刷新，失败候选继续使用旧命令。

### T58：把 Skill 加入 help 和 Tab 补全

**文件：** `internal/app/commands.go`、`internal/tui/command_menu.go`  
**依赖：** T42、T57

**步骤：**

1. 动态 Definition 使用 Skill 名称、说明、参数提示、Badge 和唯一规范命令名。
2. 单匹配补全保持现有直接补齐，多匹配菜单携带 Badge。
3. `SlashEnabled=false` 的冲突 Skill 不进入 help/补全，但仍留在 Manager 可供工具加载。

**验证：** 运行 `go test ./internal/app -run TestSkillHelpAndCompletion`，期望可用 Skill 可发现，保留命令冲突 Skill 不出现。

### T59：执行共享 Skill 斜杠命令

**文件：** `internal/app/skills.go`、`internal/app/command_controller.go`  
**依赖：** T49、T57

**步骤：**

1. 实现 Controller.ExecuteSkill，传递规范名称、完整 args 和原始输入。
2. shared Skill 先原子激活主 Activity，再以原始 `/name args` 作为主用户消息启动普通 Agent Loop。
3. SOP 只进入 Profile 系统区块；激活失败时不追加用户消息、不启动请求。

**验证：** 运行 `go test ./internal/app -run TestExecuteSharedSkillCommand`，期望历史保存原命令且 Provider 系统区块包含展开 SOP。

### T60：执行独立 Skill 斜杠命令并回流摘要

**文件：** `internal/app/skills.go`、`internal/app/command_controller.go`、`internal/app/update.go`  
**依赖：** T55、T59

**步骤：**

1. isolated Skill 在追加新命令前复制历史，然后把原始命令写入主 Conversation。
2. 启动 Independent Runner 并处理 transient 事件；成功时仅追加最终摘要 Assistant 消息。
3. 失败、取消或超时时清理临时 UI，不写成功摘要并恢复输入与默认模型状态。

**验证：** 运行 `go test ./internal/app -run TestExecuteIsolatedSkillCommand`，期望主历史只增加原始命令与最终摘要。

### T61：让 `/clear` 清除活动 Skill

**文件：** `internal/app/command_controller.go`、`internal/app/app.go`  
**依赖：** T56

**步骤：**

1. ClearMessages 在保留既有 MessagesView 清空行为后调用主 Activity.Clear。
2. 恢复默认模型和无 Skill 工具限制的状态显示。
3. 不修改 Conversation.Messages 数量、内容或上下文压缩元数据。

**验证：** 运行 `go test ./internal/app -run TestClearSkillsKeepsConversation`，期望活动状态清空而主历史逐项相等。

### T62：覆盖 App 热更新和生命周期测试

**文件：** `internal/app/skills_test.go`、`internal/app/commands_test.go`、`internal/app/update_test.go`  
**依赖：** T58、T60、T61

**步骤：**

1. 覆盖动态 help/补全、共享执行、独立执行、`/rv`、保留命令冲突和热更新失败回滚。
2. 覆盖活动 Skill 在磁盘更新后继续使用旧快照，重新激活后使用新快照。
3. 覆盖 `/clear`、新建会话、切换会话、完成、失败和取消后的状态恢复。

**验证：** 运行 `go test -race ./internal/app`，期望 App 测试通过且无数据竞争。

### T63：按严格顺序装配启动流程

**文件：** `cmd/xagent/main.go`  
**依赖：** T24、T27、T62

**步骤：**

1. 在内置和 MCP 工具注册完成后注册 `load_skill`，收集完整工具名。
2. 构造内置、`~/.config/xagent/skills/`、项目 `.xagent/skills/` 三个来源及保留命令名。
3. 在创建 Executor、Orchestrator 和 TUI 前严格创建 Manager；快照级错误打印脱敏信息并退出。

**验证：** 运行 `go test ./cmd/xagent -run TestStartupSkillAssembly`，期望有效配置装配成功，未知工具和同层冲突在 TUI 前失败。

### T64：覆盖启动路径与保留名称测试

**文件：** `cmd/xagent/main_test.go`  
**依赖：** T63

**步骤：**

1. 提取可测试的 Skill Manager 装配辅助函数，注入临时用户和项目目录。
2. 覆盖三级路径、MCP 工具可被白名单引用、`load_skill` 保留和基础命令别名冲突。
3. 在错误内容放入模拟 Secret，断言输出经过 runtime redactor。

**验证：** 运行 `go test ./cmd/xagent -run TestStartupSkillAssembly`，期望所有启动边界测试通过。

### T65：补充用户文档

**文件：** `README.md`  
**依赖：** T63

**步骤：**

1. 说明三级目录、覆盖优先级、单文件和目录型入口结构。
2. 给出全部 frontmatter 字段、`{{args}}` 示例、共享/独立差异和工具白名单语义。
3. 说明热更新、活动快照、`/clear`、内置 commit/review/test、资源只读边界和首版不支持项。

**验证：** 运行 `rg -n 'allowed_tools|shared|isolated|\{\{args\}\}|\.xagent/skills|commit|review|test' README.md`，期望所有关键主题均有可定位说明。

## 集成与质量门禁

### T66：运行领域与跨模块定向集成测试

**文件：** 全部新增和修改的 Go 文件  
**依赖：** T31、T33、T36、T43、T50、T55、T62、T64

**步骤：**

1. 运行 Skill、Tool、Prompt、Provider、Command、TUI、Orchestrator、App 和启动包测试。
2. 修复测试揭示的接口不一致、事件顺序和数据竞争，不放宽断言。
3. 重跑定向测试，确认没有依赖真实网络、真实 Provider 或交互式 TUI。

**验证：** 运行 `go test ./internal/skill ./internal/tool ./internal/prompt ./internal/provider ./internal/command ./internal/events ./internal/tui ./internal/orchestrator ./internal/app ./cmd/xagent`，期望全部包返回 `ok`。

### T67：执行格式、静态检查和全项目回归

**文件：** 全部变更文件  
**依赖：** T65、T66

**步骤：**

1. 对所有变更 Go 文件执行 gofmt，并运行 `git diff --check`。
2. 运行静态检查和全项目测试，保留既有 Memory、MCP、权限、命令、会话和 Agent Loop 用例。
3. 构建当前源码，确认内置资源被正确嵌入且无未使用接口。

**验证：** 依次运行 `gofmt -w <变更的 Go 文件>`、`git diff --check`、`go vet ./...`、`go test ./...`、`go build ./...`，期望全部成功。

### T68：运行竞态与完整验收前检查

**文件：** 全部变更文件、`docs/skill-system/checklist.md`  
**依赖：** T67

**步骤：**

1. 对 Manager、Activity、Orchestrator 和 App 运行 race 检查。
2. 对照 spec.md 的 F1–F28 和 plan.md 的模块清单，确认每项都有实现和测试证据。
3. 记录需要在 checklist.md 中人工执行的真实 TUI 场景，不用单元测试结果替代端到端观察。

**验证：** 运行 `go test -race ./internal/skill ./internal/orchestrator ./internal/app`，期望全部成功，并能为 checklist 每项提供对应证据入口。

## 执行顺序

```text
领域核心：
T1 → T2 → T3 → T4 → T5 → T6 → T7 → T8 → T9 → T10 → T11 → T12 → T13
                  └────────────→ T14 → T15 → T16 → T17 → T18
T1 → T19 → T20
T10 + T16 → T21 → T22
T3 → T23 → T24

基础集成（可与领域核心后半段并行）：
T25 → T26 → T27
T28 → T29 → T30 → T31
T32 → T33
T21 → T34 → T35 → T36
T37 → T38 → T39
T40 → T41
T37 → T42
T41 + T42 → T43

执行协调：
T17 → T44 → T45
T27 + T33 + T36 + T45 → T46 → T47
T12 + T16 + T24 → T48
T47 + T48 → T49 → T50
T19 + T48 → T51
T45 + T51 → T52
T40 + T52 → T53 → T54 → T55

App 与启动：
T12 + T16 → T56
T39 + T56 → T57
T42 + T57 → T58
T49 + T57 → T59
T55 + T59 → T60
T56 → T61
T58 + T60 + T61 → T62
T24 + T27 + T62 → T63 → T64
T63 → T65

质量门禁：
T31 + T33 + T36 + T43 + T50 + T55 + T62 + T64 → T66
T65 + T66 → T67 → T68
```

关键串行路径是：

```text
Skill 解析与快照
→ Activity/Profile
→ 工具视图与 Prompt
→ Orchestrator shared/isolated 执行
→ App 动态命令与生命周期
→ 启动装配
→ 全项目质量门禁
```

不存在反向依赖：`internal/skill` 不导入 App、TUI、Provider、Orchestrator 或 Command；Tool 和 Provider 不感知 Skill；Orchestrator 负责协调，App 只持有会话级 Activity。
