# XAgent 项目技术说明



XAgent 是一个使用 Go 构建的终端 AI 编程助手。它把大模型、代码工具、项目上下文、权限控制、会话恢复和子 Agent 统一到一个本地运行时中。

一句话概括：**模型负责提出意图，XAgent 负责把意图变成受控、可恢复的工程执行。**


## 快速了解

| 读者最关心的问题 | 答案 |
| --- | --- |
| XAgent 是什么？ | 一个运行在本地终端中的 AI 编程 Agent，也是负责模型、工具、权限、上下文和子任务的工程运行时 |
| 它面向谁？ | 需要理解代码、修改项目、执行验证、沉淀流程或拆解复杂任务的研发人员 |
| 用户给它什么？ | 自然语言任务、当前代码仓库，以及项目配置、Skill、Hook、权限规则等上下文 |
| 它能产出什么？ | 代码分析、实施计划、文件修改、命令结果、验证结论、子任务结果和可恢复的会话记录 |
| 它运行在哪里？ | 主程序、工具和数据治理主要运行在本地；模型请求和 MCP 工具可按配置访问外部服务 |
| 它和普通 AI 聊天有什么不同？ | 模型不能直接操作项目，所有副作用都要经过 XAgent 的确定性校验、权限和执行边界 |

从使用者视角看，XAgent 提供一个终端入口；从架构视角看，它是一套 Agent Runtime；从安全视角看，它是模型与真实工程环境之间的治理层。

## 1. 项目定位与价值

大模型能够理解和生成代码，但直接操作真实仓库仍存在不确定性、副作用、上下文丢失和并行冲突。XAgent 在模型与工程环境之间增加一层可治理的 Agent Runtime。

### 1.1 三方职责

| 参与者 | 负责什么 | 不负责什么 |
| --- | --- | --- |
| 用户 | 提出目标、确认高风险操作、判断最终结果 | 不需要手工拼接每次模型请求和工具结果 |
| 大模型 | 理解需求、分析代码、提出计划和工具调用意图 | 不直接持有文件系统、Shell 或 Git 的执行权 |
| XAgent | 组织上下文、暴露可用工具、校验授权、执行操作、保存状态 | 不代替用户作最终业务判断，也不把模型输出视为可信指令 |

这三者形成“用户定目标、模型做推理、运行时控执行”的协作关系。

### 1.2 这个项目做了什么

XAgent 已经把一个可对话的模型客户端扩展为完整的本地 Agent 工程系统：

| 建设内容 | 已完成的能力 | 解决的问题 |
| --- | --- | --- |
| 终端应用 | 流式回答、Thinking、工具状态、权限确认、会话与任务导航 | 让用户在一个入口内观察并控制 Agent |
| Agent Loop | 模型请求、工具调用、结果回写、停止原因和 Usage 结算 | 让模型能够连续完成多步任务，而不是只回答一次 |
| 模型适配 | Anthropic 与 OpenAI-compatible 协议、流式事件和工具 Schema | 隔离不同模型服务的协议差异 |
| 工具执行系统 | 文件、搜索、Bash、Git、Artifact、Schema、Registry 和 Executor | 把模型意图转成可校验、可限时的本地操作 |
| 权限与安全 | 权限规则、人在环确认、一次性 Ticket、路径保护、脱敏和资源上限 | 防止模型绕过确认或产生无界副作用 |
| 上下文与数据 | Token 预算、摘要压缩、JSONL 会话、恢复、指令、记忆和大型结果外置 | 支持长任务以及进程中断后的继续工作 |
| 扩展体系 | Skill、Hook 和 MCP | 复用工作流、响应生命周期事件、接入外部工具 |
| SubAgent | Defined/Fork、角色、队列、事件、取消、后台运行和结果 Inbox | 把复杂任务拆成可管理的独立子任务 |
| Worktree 隔离 | 独立任务根、Git Worktree、租约、恢复、结算和保守清理 | 避免多个 Agent 修改同一工作目录并保护未提交成果 |
| 测试与文档 | 大规模测试、故障场景、真实 Git 边界和模块化设计文档 | 让关键行为可以验证、回归和审查 |

### 1.3 为什么需要这套运行时

| 工程问题 | 平台能力 | 带来的价值 |
| --- | --- | --- |
| 模型输出不确定，可能调用错误工具 | 工具校验、权限确认、一次性执行凭证 | 模型不能直接绕过本地控制 |
| 文件修改和命令执行可能产生副作用 | 最小权限、路径保护、超时和输出上限 | 降低误操作和资源失控风险 |
| 长会话容易超出上下文或中断 | 上下文压缩、Artifact、会话恢复和记忆 | 支持更长、更连续的工程任务 |
| 团队流程和外部能力难复用 | Skill、Hook 和 MCP | 在不修改 Agent Loop 的情况下扩展能力 |
| 复杂任务拆解后容易互相覆盖 | 子 Agent、结果 Inbox 和 Git Worktree | 支持并行处理并保护已有成果 |

XAgent 的主要优势是：**闭环完整、安全边界前置、长任务可恢复、多 Agent 可隔离、扩展机制不侵入核心。**

### 1.4 项目边界

XAgent 不是新的大模型，也不负责训练模型；它负责让现有模型在代码仓库中可靠工作。Worktree 提供工程级目录隔离，但不是对抗同用户恶意进程的操作系统沙箱。系统也不会在没有用户授权的情况下自动完成代码合并、推送或发布。

### 1.5 典型使用场景

| 场景 | 用户通常怎么说 | XAgent 实际完成的工作 | 最终产出 |
| --- | --- | --- | --- |
| 理解陌生项目 | “这个模块是怎么工作的？” | 搜索入口、读取关键实现、追踪调用关系并组织证据 | 代码结构、执行流程和关键文件说明 |
| 定位问题 | “为什么这个请求会失败？” | 检索日志相关代码、分析状态变化、运行只读诊断 | 原因判断、影响范围和验证方法 |
| 修改代码 | “修复这个 Bug，并补充测试” | 制定步骤、编辑文件、执行测试、根据结果继续修正 | 代码改动、测试结果和变更说明 |
| 执行重复流程 | “按团队规范检查这次变更” | 加载 Skill 或项目指令，按固定流程调用工具 | 可重复的检查过程和结构化结论 |
| 拆解复杂任务 | “并行分析四个相互独立的模块” | 创建子任务、分配上下文和能力、汇总结果 | 多个子 Agent 的独立结论与统一摘要 |
| 隔离并行修改 | “让不同子任务分别修改代码” | 为任务绑定独立 Worktree，保留未提交成果 | 互不覆盖的修改现场，供用户审查和集成 |
| 继续中断任务 | “恢复上次会话继续处理” | 校验持久化记录、恢复可信状态和上下文摘要 | 可继续执行的会话，而不是从头重做 |

## 2. 总体架构

```mermaid
%%{init: {'theme':'base','themeVariables':{'primaryColor':'#e0f2fe','primaryTextColor':'#0f172a','primaryBorderColor':'#0ea5e9','lineColor':'#64748b','secondaryColor':'#ecfdf5','tertiaryColor':'#f5f3ff','clusterBkg':'#f8fafc','clusterBorder':'#cbd5e1','fontFamily':'PingFang SC, sans-serif'}}}%%
flowchart LR
    U["用户"] --> UI["CLI / TUI"]

    subgraph R["XAgent 本地运行时"]
        UI --> APP["App 状态与交互"]
        APP --> ORC["Orchestrator\nAgent Loop"]
        ORC --> CTX["Prompt / Context"]
        ORC --> PRV["Provider Adapter"]
        ORC --> TV["Tool View"]
        TV --> SEC["校验 → 权限 → Ticket"]
        SEC --> EXE["Tool Executor"]
        ORC --> SUB["SubAgent Service"]
        SUB --> WT["Workspace / Worktree"]
        CTX <--> DATA["Session / Memory / Artifact"]
    end

    PRV --> LLM["模型服务"]
    EXE --> LOCAL["文件 / Bash / Git"]
    EXE --> MCP["MCP Server"]

    classDef user fill:#e0f2fe,stroke:#0ea5e9,color:#0c4a6e,stroke-width:1.5px;
    classDef runtime fill:#ecfdf5,stroke:#10b981,color:#064e3b,stroke-width:1.5px;
    classDef security fill:#fff1f2,stroke:#fb7185,color:#881337,stroke-width:1.5px;
    classDef external fill:#f5f3ff,stroke:#8b5cf6,color:#4c1d95,stroke-width:1.5px;
    class U,UI user;
    class APP,ORC,CTX,PRV,TV,EXE,SUB,WT,DATA runtime;
    class SEC security;
    class LLM,LOCAL,MCP external;
```

### 2.1 六层组成

| 层次 | 负责什么 | 代表模块 |
| --- | --- | --- |
| 交互层 | 输入、流式展示、确认和任务导航 | `app`、`tui` |
| 编排层 | Agent Loop、停止条件和任务运行 | `orchestrator` |
| 能力层 | 模型、工具、Skill、Hook、MCP、SubAgent | `provider`、`tool`、`subagent` |
| 治理层 | 权限、路径、脱敏和资源限制 | `permission`、`safefs`、`redact` |
| 数据层 | 会话、上下文、记忆和大型结果 | `conversation`、`contextmgr`、`artifact` |
| 隔离层 | 按任务根重建能力和管理 Git Worktree | `workspace`、`worktree` |

### 2.2 三条关键链路

为了理解这些模块如何配合，可以把系统看成三条同时工作的链路：

```mermaid
%%{init: {'theme':'base','flowchart':{'curve':'basis'},'themeVariables':{'primaryTextColor':'#0f172a','lineColor':'#64748b','fontFamily':'PingFang SC, sans-serif'}}}%%
flowchart TB
    subgraph REQUEST["任务执行链"]
        direction LR
        U["用户任务"] --> O["Agent Loop"] --> M["模型推理"] --> T["工具执行"] --> O
    end

    subgraph CONTROL["安全治理链"]
        direction LR
        V["参数校验"] --> P["权限判断"] --> K["一次性 Ticket"] --> B["超时与资源边界"]
    end

    subgraph STATE["状态数据链"]
        direction LR
        C["会话记录"] --> X["上下文预算"] --> A["Artifact 外置"] --> R["恢复与记忆"]
    end

    T -. "执行前必须通过" .-> V
    O -. "读取与写入" .-> C
    B -. "约束" .-> T
    R -. "为后续请求提供上下文" .-> O

    classDef request fill:#ecfdf5,stroke:#10b981,color:#064e3b,stroke-width:1.5px;
    classDef control fill:#fff1f2,stroke:#fb7185,color:#881337,stroke-width:1.5px;
    classDef state fill:#eff6ff,stroke:#3b82f6,color:#1e3a8a,stroke-width:1.5px;
    class U,O,M,T request;
    class V,P,K,B control;
    class C,X,A,R state;
    style REQUEST fill:#f0fdf4,stroke:#6ee7b7
    style CONTROL fill:#fff1f2,stroke:#fda4af
    style STATE fill:#eff6ff,stroke:#93c5fd
```

- **任务执行链**让模型能够反复“思考—操作—观察”，直到得到结果或命中停止条件。
- **安全治理链**决定某次操作是否允许以及允许到什么范围，不能被模型输出绕过。
- **状态数据链**控制哪些信息进入模型上下文，并保存能够安全恢复的工程状态。

## 3. Agent 是怎么工作的

### 3.1 一次请求的完整闭环

```mermaid
%%{init: {'theme':'base','themeVariables':{'actorBkg':'#e0f2fe','actorBorder':'#0284c7','actorTextColor':'#0f172a','signalColor':'#475569','signalTextColor':'#0f172a','activationBkgColor':'#dcfce7','activationBorderColor':'#10b981','noteBkgColor':'#fff7ed','noteBorderColor':'#fb923c','fontFamily':'PingFang SC, sans-serif'}}}%%
sequenceDiagram
    participant U as 用户
    participant A as XAgent
    participant P as 模型
    participant E as 工具执行边界

    U->>A: 提交任务
    A->>P: 上下文 + 可见工具
    P-->>A: 文本或工具调用

    alt 模型直接回答
        A-->>U: 流式展示并保存
    else 模型请求工具
        A->>E: 校验、授权、限时执行
        E-->>A: 有界且脱敏的结果
        A->>P: 回写结果，继续推理
    end

    A-->>U: 返回结果和明确停止原因
```

Agent Loop 只负责连接“模型推理—工具执行—结果回写”。达到完成、错误、取消、权限拒绝或资源上限时，循环会明确停止。

具体来说，一轮任务会经过下面几个阶段：

| 阶段 | XAgent 做什么 | 为什么需要 |
| --- | --- | --- |
| 1. 准备任务 | 绑定当前项目根、会话、运行模式、Skill 和可用工具 | 避免把其他项目或角色的能力带入本次任务 |
| 2. 组织上下文 | 组合系统提示、用户请求、历史消息、项目指令和子任务结果 | 给模型足够信息，同时遵守 Token 与字节预算 |
| 3. 请求模型 | 通过 Provider Adapter 发起流式请求并统一文本、Thinking、Usage 和工具调用事件 | 屏蔽不同模型协议的差异 |
| 4. 判断响应 | 文本直接展示；工具调用进入 Schema、Registry、能力和权限检查 | 模型的工具调用只是请求，不是执行命令 |
| 5. 有界执行 | 在项目根和权限范围内执行工具，并施加超时、取消、输出和进程边界 | 控制文件、Shell、Git 等真实副作用 |
| 6. 结果回写 | 将结构化、脱敏、必要时外置的工具结果返回模型 | 让模型根据真实执行结果继续判断，而不是猜测 |
| 7. 结束或继续 | 模型完成则保存结果；仍需操作则进入下一轮；异常则记录明确停止原因 | 使任务行为可观察、可恢复、可诊断 |

### 3.2 一个代码修复任务如何完成

下面以“定位并修复一个 Bug”为例。XAgent 不会一次生成答案后直接结束，而是围绕真实仓库形成证据闭环：

```mermaid
%%{init: {'theme':'base','flowchart':{'curve':'basis'},'themeVariables':{'primaryColor':'#e0f2fe','primaryTextColor':'#0f172a','primaryBorderColor':'#0ea5e9','lineColor':'#64748b','fontFamily':'PingFang SC, sans-serif'}}}%%
flowchart LR
    A["理解问题"] --> B["搜索入口与调用链"]
    B --> C["形成原因假设"]
    C --> D["读取测试与约束"]
    D --> E["修改最小范围代码"]
    E --> F["运行目标测试"]
    F --> G{"验证通过？"}
    G -->|否| H["读取失败结果并修正"]
    H --> E
    G -->|是| I["检查差异并总结"]

    classDef discover fill:#e0f2fe,stroke:#0ea5e9,color:#0c4a6e,stroke-width:1.5px;
    classDef act fill:#ecfdf5,stroke:#10b981,color:#064e3b,stroke-width:1.5px;
    classDef decide fill:#fff7ed,stroke:#fb923c,color:#7c2d12,stroke-width:1.5px;
    class A,B,C,D discover;
    class E,F,H,I act;
    class G decide;
```

这里真正执行搜索、编辑和测试的是受控工具；模型负责决定下一步，XAgent 负责保证每一步只在当前任务允许的边界内发生。用户可以看到过程、拒绝高风险操作，也可以随时取消。

### 3.3 复杂任务如何拆解

```mermaid
%%{init: {'theme':'base','themeVariables':{'primaryColor':'#ecfdf5','primaryTextColor':'#0f172a','primaryBorderColor':'#10b981','lineColor':'#64748b','secondaryColor':'#eff6ff','tertiaryColor':'#fff7ed','fontFamily':'PingFang SC, sans-serif'}}}%%
flowchart LR
    P["父 Agent"] --> S["提交子任务"]
    S --> R{"上下文来源"}
    R -->|Defined| D["独立角色"]
    R -->|Fork| F["父会话快照"]
    D --> I{"是否隔离"}
    F --> I
    I -->|共享| W1["任务 Workspace"]
    I -->|隔离| W2["Git Worktree"]
    W1 --> X["子 Agent 执行"]
    W2 --> X
    X --> Q["结果进入 Inbox"]
    Q --> P
    W2 --> C{"可以安全清理？"}
    C -->|不能证明| KEEP["保留现场"]
    C -->|确认无成果| CLEAN["清理临时目录"]

    classDef parent fill:#e0f2fe,stroke:#0ea5e9,color:#0c4a6e,stroke-width:1.5px;
    classDef task fill:#ecfdf5,stroke:#10b981,color:#064e3b,stroke-width:1.5px;
    classDef decision fill:#fff7ed,stroke:#fb923c,color:#7c2d12,stroke-width:1.5px;
    classDef protect fill:#fff1f2,stroke:#fb7185,color:#881337,stroke-width:1.5px;
    class P,Q parent;
    class S,D,F,W1,W2,X,CLEAN task;
    class R,I,C decision;
    class KEEP protect;
```

后台子 Agent 的结果不会偷偷触发新的父模型轮次，而是在下一个安全请求边界进入父上下文。使用 Worktree 时，无法证明可以安全删除就保留现场。

两种子 Agent 上下文模式解决不同问题：

| 模式 | 子 Agent 获得的上下文 | 适合什么任务 | 主要特点 |
| --- | --- | --- | --- |
| Defined | 预先定义的角色、指令、模型和工具范围 | 固定职责，如测试分析、代码审阅 | 输入稳定、能力清晰、便于重复使用 |
| Fork | 父任务在安全边界形成的上下文快照 | 与当前讨论紧密相关的临时分析 | 减少重复说明，但仍重新计算工具和权限边界 |

子 Agent 还可以选择两种工作目录策略：共享 Workspace 适合只读分析或明确不会冲突的任务；Git Worktree 适合需要独立修改代码的任务。隔离任务的成果不会自动 merge、commit 或 push，最终仍由用户决定如何集成。

## 4. 功能组成

```mermaid
%%{init: {'theme':'base','flowchart':{'curve':'basis','nodeSpacing':28,'rankSpacing':38},'themeVariables':{'primaryTextColor':'#0f172a','lineColor':'#64748b','clusterBkg':'#f8fafc','clusterBorder':'#cbd5e1','fontFamily':'PingFang SC, sans-serif'}}}%%
flowchart TB
    subgraph ACCESS["交互与模型入口"]
        direction LR
        UI["终端交互\n流式回答 · Thinking · 权限确认"]
        MODEL["模型接入\nAnthropic · OpenAI-compatible"]
    end

    subgraph RUNTIME["Agent Runtime 核心闭环"]
        direction LR
        LOOP["Agent Loop\n任务编排 · 停止条件 · Usage"]
        TOOLS["代码工具\n文件 · 搜索 · Bash · Git · Artifact"]
        CONTEXT["上下文管理\nToken 预算 · 摘要压缩 · 结果外置"]
        DATA["会话与记忆\nJSONL 恢复 · 项目指令 · 长期记忆"]

        LOOP <--> TOOLS
        LOOP <--> CONTEXT
        CONTEXT <--> DATA
    end

    subgraph GOVERN["治理平面"]
        direction LR
        PERMISSION["权限治理\n规则 · 人在环确认 · Ticket"]
        SAFETY["运行保护\n路径 · 超时 · 限额 · 脱敏"]
    end

    subgraph EXTEND["扩展与多 Agent"]
        direction LR
        SKILL["Skill\n复用操作流程"]
        HOOK["Hook\n生命周期自动化"]
        MCP["MCP\n接入外部工具"]
        SUB["SubAgent\nDefined / Fork · Queue · Inbox"]
        WT["Git Worktree\n独立任务根 · 成果保护"]
    end

    UI -->|"任务 / 确认"| LOOP
    MODEL <-->|"推理 / 流式响应"| LOOP
    PERMISSION -.-> LOOP
    PERMISSION -.-> TOOLS
    SAFETY -.-> LOOP
    SAFETY -.-> TOOLS
    SKILL --> LOOP
    HOOK --> LOOP
    MCP --> TOOLS
    LOOP --> SUB
    SUB -. "可选隔离" .-> WT

    classDef access fill:#e0f2fe,stroke:#0ea5e9,color:#0c4a6e,stroke-width:1.5px;
    classDef core fill:#ecfdf5,stroke:#10b981,color:#064e3b,stroke-width:1.8px;
    classDef data fill:#f5f3ff,stroke:#8b5cf6,color:#4c1d95,stroke-width:1.5px;
    classDef govern fill:#fff1f2,stroke:#fb7185,color:#881337,stroke-width:1.5px;
    classDef extend fill:#fff7ed,stroke:#fb923c,color:#7c2d12,stroke-width:1.5px;
    classDef isolate fill:#fffbeb,stroke:#f59e0b,color:#78350f,stroke-width:1.8px;

    class UI,MODEL access;
    class LOOP,TOOLS core;
    class CONTEXT,DATA data;
    class PERMISSION,SAFETY govern;
    class SKILL,HOOK,MCP,SUB extend;
    class WT isolate;

    style ACCESS fill:#f0f9ff,stroke:#7dd3fc,stroke-width:1px
    style RUNTIME fill:#f0fdf4,stroke:#6ee7b7,stroke-width:1px
    style GOVERN fill:#fff1f2,stroke:#fda4af,stroke-width:1px,stroke-dasharray:5 4
    style EXTEND fill:#fffaf0,stroke:#fdba74,stroke-width:1px
```

绿色区域构成 Agent 的主运行闭环；红色治理平面对模型、工具和资源施加约束；橙色区域负责流程扩展与复杂任务拆解。所有扩展最终仍进入同一 Agent Loop 和工具执行边界，不能自行提高本地权限。

### 4.1 用户能够直接使用的能力

| 功能域 | 系统提供的能力 | 用户能够感知到的效果 |
| --- | --- | --- |
| 终端交互 | 流式文本、Thinking 状态、工具进度、权限确认、取消和任务切换 | 能看到 Agent 正在做什么，并在关键操作前作决定 |
| 代码工具 | 文件读取与编辑、文本搜索、Bash、Git 和结构化 Artifact | Agent 能基于真实代码执行分析、修改和验证 |
| 多模型接入 | Anthropic 与 OpenAI-compatible Provider Adapter | 上层 Agent Loop 不依赖单一模型协议 |
| 上下文管理 | 请求预算、历史压缩、工具结果外置和稳定提示准备 | 长会话不会简单地把所有历史无限塞给模型 |
| 会话与记忆 | JSONL 持久化、记录校验、会话恢复、项目指令和长期记忆 | 中断后可以继续，并保留对项目有价值的信息 |
| 权限治理 | 规则匹配、人在环确认、一次性 Ticket 和能力收窄 | 高风险工具不能仅凭模型一句话直接执行 |
| Skill | 按需加载可复用说明、脚本、模板和资源 | 团队流程可以被反复调用，不必每次重新描述 |
| Hook | 在系统、会话、轮次、消息、工具和压缩等事件上运行自动化 | 统一执行审计、记录或项目级约束 |
| MCP | 发现、连接并调用外部 MCP Server 提供的工具 | 在统一工具边界内使用外部系统能力 |
| SubAgent | 子任务提交、排队、并发、取消、事件和结果 Inbox | 复杂任务可以拆分，但父任务仍能统一收口 |
| Workspace / Worktree | 按任务重建工具、路径、Hook 和数据根；可选 Git 隔离 | 子任务不会天然继承所有能力，并可避免并行写冲突 |

### 4.2 四种扩展方式如何选择

| 需要解决的问题 | 选择 | 原因 |
| --- | --- | --- |
| 复用一套可由 Agent 阅读和执行的工作方法 | Skill | 适合封装说明、脚本、模板和渐进式上下文 |
| 在固定生命周期节点自动检查或记录 | Hook | 不依赖模型主动想起，适合确定性的横切流程 |
| 连接数据库、平台或其他外部系统工具 | MCP | 使用标准协议把外部能力纳入工具目录和权限边界 |
| 把复杂目标拆给独立执行单元 | SubAgent | 每个任务有自己的上下文、状态、能力和完成结果 |

这些机制彼此可以组合。例如，一个子 Agent 可以在独立 Worktree 中工作，加载特定 Skill，通过 MCP 查询外部信息，并仍然接受 Hook 和权限系统约束。

## 5. 我如何通过 Spec 实现 XAgent

XAgent 不是先把所有代码写完，再补一份说明文档。每项较大的能力都会先建立 `spec.md → plan.md → task.md → checklist.md` 四份文件，把需求、设计、实现和验收串成一条可追踪链路。

```mermaid
flowchart LR
    R["问题与目标"] --> S["spec.md\n需求、边界、验收标准"]
    S --> G["评审与确认"]
    G --> P["plan.md\n架构、接口、数据流"]
    P --> T["task.md\n有序任务、文件范围"]
    T --> C["checklist.md\n定义验收项与记录方式"]
    C --> I["实现代码与测试\n逐项更新 Checklist"]
    I --> D{"全部满足？"}
    D -->|"否"| F["定位差距并回写\nSpec / Plan / Task"]
    F --> G
    D -->|"是"| B["形成可回归基线"]

    classDef define fill:#e0f2fe,stroke:#0ea5e9,color:#0c4a6e,stroke-width:1.5px;
    classDef design fill:#f5f3ff,stroke:#8b5cf6,color:#4c1d95,stroke-width:1.5px;
    classDef build fill:#ecfdf5,stroke:#10b981,color:#064e3b,stroke-width:1.5px;
    classDef verify fill:#fff7ed,stroke:#fb923c,color:#7c2d12,stroke-width:1.5px;
    class R,S,G define;
    class P,T design;
    class I,B build;
    class C,D,F verify;
```

### 5.1 四份文档分别解决什么问题

| 阶段 | 核心问题 | 文档中的主要内容 | 对实现的约束 |
| --- | --- | --- | --- |
| `spec.md` | 为什么做、要做到什么 | 背景、目标、功能需求、非功能需求、不做的事、验收标准 | 防止需求边界在编码过程中漂移 |
| `plan.md` | 准备怎么做 | 模块拆分、核心类型、接口、调用链、状态机、并发与错误策略 | 先确定模块关系和失败行为，再决定文件改动 |
| `task.md` | 按什么顺序落地 | 文件清单、里程碑、有序任务、每项任务的测试要求 | 把大设计拆成可独立实现和验证的小步骤 |
| `checklist.md` | 如何证明已经完成 | 功能、异常、安全、集成、端到端和人工验收项 | 未通过或未执行的项目继续保留，不能仅凭“代码已写”宣布完成 |

四份文档不是彼此独立的总结，而是逐层收窄：Spec 中的需求和验收标准进入 Plan，Plan 中的模块设计被拆到 Task，Task 的结果最终回到 Checklist。实现中发现的新边界也需要回写前面的文档，而不是让代码与设计长期分叉。

### 5.2 在项目中的实际落地

仓库当前保留了 15 组同时包含这四份文件的设计目录，覆盖核心循环、工具与权限、上下文与恢复、扩展系统以及多 Agent 隔离。下面列出主要对应关系：

| 能力范围 | 代表 Spec | 主要实现模块 |
| --- | --- | --- |
| 模型与核心循环 | [Agent Loop](https://github.com/gugugu5331/XAgent/blob/main/docs/agent-loop/spec.md)、[System Prompt](https://github.com/gugugu5331/XAgent/blob/main/docs/system-prompt/spec.md) | `orchestrator`、`provider`、`prompt`、`events` |
| 工具与权限 | [Tool System](https://github.com/gugugu5331/XAgent/blob/main/docs/tool-system/spec.md)、[Permission System](https://github.com/gugugu5331/XAgent/blob/main/docs/permission-system/spec.md) | `tool`、`permission`、`safefs`、`proctree` |
| 上下文与恢复 | [Context Management](https://github.com/gugugu5331/XAgent/blob/main/docs/context-management/spec.md)、[Session Memory Restore](https://github.com/gugugu5331/XAgent/blob/main/docs/session-memory-restore/spec.md) | `contextmgr`、`conversation`、`sessionctx`、`memory`、`artifact` |
| 扩展机制 | [Command](https://github.com/gugugu5331/XAgent/blob/main/docs/command-system/spec.md)、[Skill](https://github.com/gugugu5331/XAgent/blob/main/docs/skill-system/spec.md)、[Hook](https://github.com/gugugu5331/XAgent/blob/main/docs/hook-system/spec.md)、[MCP](https://github.com/gugugu5331/XAgent/blob/main/docs/mcp-client/spec.md) | `command`、`skill`、`hook`、`mcpclient` |
| 多 Agent 与隔离 | [SubAgent](https://github.com/gugugu5331/XAgent/blob/main/docs/subagent-system/spec.md)、[Worktree](https://github.com/gugugu5331/XAgent/blob/main/docs/worktree-isolation/spec.md) | `subagent`、`agentrole`、`workspace`、`worktree` |

以权限系统为例，[Spec](https://github.com/gugugu5331/XAgent/blob/main/docs/permission-system/spec.md) 先规定 `allow / deny / ask`、规则优先级、Plan Mode 硬约束和 fail-closed 行为；[Plan](https://github.com/gugugu5331/XAgent/blob/main/docs/permission-system/plan.md) 再把决策集中到 `permission.Authorizer`，要求 Executor 使用一次性授权凭证；[Task](https://github.com/gugugu5331/XAgent/blob/main/docs/permission-system/task.md) 按“离线权限核心 → 执行链接入 → TUI 交互 → 端到端验收”推进；[Checklist](https://github.com/gugugu5331/XAgent/blob/main/docs/permission-system/checklist.md) 最后逐项记录黑名单、路径、规则、确认和失败场景是否满足。这样可以从一条验收项一直追到设计决策、实现包和测试场景。

需要特别区分：**存在四份文档只代表需求链路已经建立，不代表所有验收都已完成。** 真实状态以对应 `checklist.md` 中的完成项、未完成项和保存证据为准。

## 6. Agent 各模块架构

前面的总体架构说明 XAgent 有哪些层，本章进一步说明代码模块如何拆分、各自负责什么，以及它们怎样组成一次完整的 Agent 执行。

### 6.1 模块关系总览

```mermaid
flowchart TB
    CMD["cmd/xagent\n启动与依赖装配"] --> APP["app / tui / command\n交互与应用状态"]
    APP <--> ORC["orchestrator / events\nAgent Loop 与事件流"]

    ORC <--> MODEL["provider / prompt / resources\n模型协议与提示组织"]
    ORC <--> STATE["conversation / contextmgr / sessionctx\nmemory / instructions / artifact"]
    ORC <--> EXT["skill / hook / mcpclient\n扩展能力"]
    ORC --> TOOL["tool\nRegistry / View / Executor"]
    TOOL --> GUARD["permission / safefs / proctree\nredact / budget"]
    MODEL --> NET["netpolicy\n外部网络策略"]
    EXT --> NET
    EXT --> TOOL

    ORC --> SUB["subagent / agentrole\n子任务与角色"]
    SUB --> WS["workspace\n任务级运行时"]
    WS --> WT["worktree\nGit 隔离与成果保护"]
    WS --> TOOL

    BASE["config / diagnostics / matcher\nrepoaudit 等基础模块"] -. "配置、诊断与公共约束" .-> APP
    BASE -.-> ORC
    BASE -.-> EXT
    BASE -.-> GUARD

    classDef entry fill:#e0f2fe,stroke:#0ea5e9,color:#0c4a6e,stroke-width:1.5px;
    classDef core fill:#ecfdf5,stroke:#10b981,color:#064e3b,stroke-width:1.6px;
    classDef data fill:#f5f3ff,stroke:#8b5cf6,color:#4c1d95,stroke-width:1.5px;
    classDef guard fill:#fff1f2,stroke:#fb7185,color:#881337,stroke-width:1.5px;
    classDef extend fill:#fff7ed,stroke:#fb923c,color:#7c2d12,stroke-width:1.5px;
    class CMD,APP entry;
    class ORC,TOOL core;
    class MODEL,STATE data;
    class GUARD,NET guard;
    class EXT,SUB,WS,WT extend;
```

[`cmd/xagent`](https://github.com/gugugu5331/XAgent/tree/main/cmd/xagent) 是组合根：它按配置、安全、执行、适配器、编排和 UI 六个阶段构造依赖。上层模块通过接口使用下层能力，避免由 TUI 或模型适配器直接创建工具、权限或存储对象。

### 6.2 核心运行模块

| 模块 | 内部架构 | 与其他模块的关系 |
| --- | --- | --- |
| [`app`](https://github.com/gugugu5331/XAgent/tree/main/internal/app) | Bubble Tea 应用状态、请求状态、会话列表、确认状态和任务导航 | 接收 TUI 输入，调用 Orchestrator，并把事件投影成界面状态 |
| [`tui`](https://github.com/gugugu5331/XAgent/tree/main/internal/tui) | 聊天、帮助、任务详情、状态栏等纯展示组件 | 只消费经过整理的状态和安全文本，不直接调用模型或执行工具 |
| [`command`](https://github.com/gugugu5331/XAgent/tree/main/internal/command) | 命令定义、Registry、解析、补全和 Controller 接口 | 把 `/plan`、`/status`、`/agent` 等输入转换为应用意图 |
| [`orchestrator`](https://github.com/gugugu5331/XAgent/tree/main/internal/orchestrator) | `Orchestrator`、Agent Loop、流收集、工具分批、停止条件和结果投影 | 是运行时中心，连接 Provider、上下文、工具、权限、Hook、Skill 和 SubAgent |
| [`events`](https://github.com/gugugu5331/XAgent/tree/main/internal/events) | 统一的文本、Thinking、工具、确认、Usage、进度和完成事件 | 解耦 Orchestrator 与 App/TUI，使后台执行不阻塞界面循环 |
| [`provider`](https://github.com/gugugu5331/XAgent/tree/main/internal/provider) | Provider 接口、Anthropic/OpenAI-compatible Adapter、SSE 解码和统一 `ChatStream` | 把不同模型协议转换为相同的流事件、工具调用和 Usage |
| [`prompt`](https://github.com/gugugu5331/XAgent/tree/main/internal/prompt) / [`resources`](https://github.com/gugugu5331/XAgent/tree/main/internal/resources) | 稳定块、动态块、Skill 目录、Hook 注入和内置提示资源 | 为 Provider 请求构造有顺序、有范围的系统上下文 |
| [`tool`](https://github.com/gugugu5331/XAgent/tree/main/internal/tool) | Schema、Registry、只读/角色 View、Executor、结果工厂和内置工具 | 向模型暴露受限能力，把已授权调用变成文件、搜索和 Bash 操作，并在需要时将大结果外置为 Artifact |

一次主任务由 App 提交给 Orchestrator；Orchestrator 调用 Provider 获得模型响应。如果响应包含工具调用，Orchestrator 再依次经过 Tool Registry、Permission 和 Executor 完成校验、授权与执行，并把结果写入 Conversation 后进入下一轮。Provider 只负责模型协议，不会直接调用 Tool Registry。Agent Loop 决定继续或以明确原因停止，所有进度通过 `events` 返回 App 和 TUI。

### 6.3 状态、安全与扩展模块

| 模块域 | 包 | 架构职责 |
| --- | --- | --- |
| 会话与上下文 | [`conversation`](https://github.com/gugugu5331/XAgent/tree/main/internal/conversation)、[`contextmgr`](https://github.com/gugugu5331/XAgent/tree/main/internal/contextmgr)、[`sessionctx`](https://github.com/gugugu5331/XAgent/tree/main/internal/sessionctx) | Conversation 保存消息与工具结果；Context Manager 计算预算、压缩和结果外置；Session Context 组合指令、记忆与稳定提示 |
| 指令、记忆与大型结果 | [`instructions`](https://github.com/gugugu5331/XAgent/tree/main/internal/instructions)、[`memory`](https://github.com/gugugu5331/XAgent/tree/main/internal/memory)、[`artifact`](https://github.com/gugugu5331/XAgent/tree/main/internal/artifact) | 安全加载项目指令，维护可检索记忆，将大结果保存为不暴露真实路径的 Artifact 引用 |
| 权限与文件边界 | [`permission`](https://github.com/gugugu5331/XAgent/tree/main/internal/permission)、[`safefs`](https://github.com/gugugu5331/XAgent/tree/main/internal/safefs)、[`matcher`](https://github.com/gugugu5331/XAgent/tree/main/internal/matcher) | Authorizer 给出 allow/deny/ask，Ticket 约束单次执行，SafeFS 校验根与路径身份，Matcher 提供确定性规则匹配 |
| 进程、网络与资源 | [`proctree`](https://github.com/gugugu5331/XAgent/tree/main/internal/proctree)、[`netpolicy`](https://github.com/gugugu5331/XAgent/tree/main/internal/netpolicy)、[`budget`](https://github.com/gugugu5331/XAgent/tree/main/internal/budget)、[`redact`](https://github.com/gugugu5331/XAgent/tree/main/internal/redact) | 管理子进程取消与保护计划、限制网络端点、统一字节/条目预算，并在展示和持久化前脱敏 |
| Skill | [`skill`](https://github.com/gugugu5331/XAgent/tree/main/internal/skill) | 发现并解析 Markdown SOP，生成不可变快照，按 shared/isolated 模式收窄模型和工具能力 |
| Hook | [`hook`](https://github.com/gugugu5331/XAgent/tree/main/internal/hook) | 加载声明式生命周期规则，匹配事件并运行 Command、HTTP 或 Prompt 等动作；工具 Hook 仍不能绕过权限边界 |
| MCP | [`mcpclient`](https://github.com/gugugu5331/XAgent/tree/main/internal/mcpclient) | 管理 stdio/HTTP 传输、JSON-RPC 会话、工具发现、命名、租约和关闭，并把远程工具注册进统一 Tool Registry |
| 子 Agent | [`subagent`](https://github.com/gugugu5331/XAgent/tree/main/internal/subagent)、[`agentrole`](https://github.com/gugugu5331/XAgent/tree/main/internal/agentrole) | 管理 Defined/Fork 任务、队列、取消、事件、确认和 Result Inbox；角色快照决定模型、指令、工具和隔离声明 |
| 任务隔离 | [`workspace`](https://github.com/gugugu5331/XAgent/tree/main/internal/workspace)、[`worktree`](https://github.com/gugugu5331/XAgent/tree/main/internal/worktree) | Workspace 按任务根重建工具、权限、Hook、指令和数据依赖；Worktree 管理 Git 隔离、租约、恢复、结算和保守清理 |

### 6.4 基础支撑模块

| 包 | 作用 |
| --- | --- |
| [`config`](https://github.com/gugugu5331/XAgent/tree/main/internal/config) | 解析分层配置、默认值和各模块运行参数，是启动装配的输入 |
| [`diagnostics`](https://github.com/gugugu5331/XAgent/tree/main/internal/diagnostics) | 收集有界、可脱敏的诊断，不让错误信息本身成为泄漏或资源风险 |
| [`repoaudit`](https://github.com/gugugu5331/XAgent/tree/main/internal/repoaudit) | 对仓库变更、保留成果和 Worktree 状态形成可验证的审计结果 |
| [`testutil`](https://github.com/gugugu5331/XAgent/tree/main/internal/testutil) | 提供 Fake Provider、Conversation、Tool、Artifact 等测试夹具，不进入生产运行链 |

模块拆分遵循四个原则：**Orchestrator 负责编排但不直接实现工具；Provider 只适配协议而不决定权限；所有副作用必须经过确定性治理；子 Agent 的能力和数据根必须按任务重新构造。** 这样新增模型、工具或扩展时，不需要把安全规则和状态管理复制到每个接入点。

## 7. 可靠性保障与 Agent 评测

XAgent 的评测分为两个层次：第一层验证运行时本身是否安全、稳定、可恢复；第二层评测模型与 XAgent 组合后，能否稳定完成真实任务。

### 7.1 XAgent 如何保证可靠性

XAgent 不把模型输出直接当成可执行命令，而是通过确定性运行时逐层校验和约束：

```mermaid
flowchart LR
    M["模型意图"] --> V["工具与参数校验"]
    V --> P["能力收窄与权限确认"]
    P --> E["超时、输出和资源受限执行"]
    E --> S["路径保护与结果脱敏"]
    S --> R["状态记录、恢复或保守清理"]

    classDef intent fill:#fff1f2,stroke:#fb7185,color:#881337,stroke-width:1.8px;
    classDef guard fill:#fff7ed,stroke:#fb923c,color:#7c2d12,stroke-width:1.5px;
    classDef execute fill:#ecfdf5,stroke:#10b981,color:#064e3b,stroke-width:1.5px;
    classDef recover fill:#eff6ff,stroke:#3b82f6,color:#1e3a8a,stroke-width:1.5px;
    class M intent;
    class V,P guard;
    class E,S execute;
    class R recover;
```

项目通过单元测试、模块集成、并发与故障测试、真实 Git/Worktree 场景和平台差异测试验证这些机制。测试覆盖 Agent 核心、工具与安全、状态与恢复、协议与扩展、多 Agent 隔离、装配与终端体验六个方向，并重点检查超时、取消、损坏输入、竞态、资源上限和清理失败。

当前可量化的工程数据如下：

| 量化项 | 当前数据 | 含义 |
| --- | ---: | --- |
| 生产 Go 代码 | 371 个文件，80,560 行有效代码 | Agent Runtime 的实现规模 |
| Go 测试代码 | 312 个文件，90,816 行有效代码 | 确定性测试资产规模 |
| 测试与生产代码比 | 1.13:1，测试代码占 53.0% | 测试资产与生产实现的静态比例 |
| 测试入口 | 1,583 个 Test、1 个 Fuzz、1 个 TestMain | 可独立执行的测试入口规模 |
| 测试场景 | 6 类功能方向、8 类异常风险 | 当前测试覆盖的工程场景宽度 |

这里的 **53.0% 是测试代码的静态占比，不是运行时覆盖率**。当前文档也没有把历史测试记录当作当前分支的通过证据；全量测试、Race、构建和语句覆盖率都需要在固定版本上实际执行后单独记录。详细测试说明见 [测试与可靠性保障](https://github.com/gugugu5331/XAgent/blob/main/docs/testing-reliability.md)。

### 7.2 Agent 如何评测和量化能力（未来工作）

> 以下是需要结合 XAgent 实际应用场景验证的初始方案。

Agent 评测的对象是“模型 + Prompt + XAgent + 工具 + 运行环境”组成的完整系统，重点判断任务是否真正完成，以及过程是否安全、稳定、成本可控。

```mermaid
flowchart LR
    A["选择真实任务"] --> B["定义验收规则"]
    B --> C["隔离并重复执行"]
    C --> D["自动检查 + 人工复核"]
    D --> E["汇总指标并回归"]
```

实际执行流程如下：

1. **建立任务集**：先选择 10～20 个真实场景，例如代码修改、缺陷修复、测试补充和文档生成；每个任务保存代码版本、输入要求、允许修改范围和预期结果。
2. **定义验收规则**：提前明确测试或构建是否通过、目标文件是否正确、是否存在越界修改，以及哪些行为必须判为失败。
3. **隔离重复执行**：在临时仓库或 Worktree 中将每个任务重复运行 3 次，并固定模型、Prompt、Skill 和工具版本；保存执行 Trace、代码 Diff、测试结果、耗时和 Token。
4. **判定结果**：可自动验证的内容由测试、构建和 Diff 检查评分，无法自动判断的内容再人工复核。只要出现权限绕过、敏感信息泄漏、越界修改或误删成果，该次任务直接失败。
5. **形成回归基线**：汇总任务成功率、重复稳定率、风险事件率和 P95 耗时/Token；版本升级后使用同一任务集重跑，失败案例补充到回归集。

一次执行只有在功能验收通过且未触发安全硬门槛时，才记为成功。建议记录以下量化指标：

| 量化指标 | 计算口径 | 主要说明 |
| --- | --- | --- |
| **任务成功率** | 成功执行次数 ÷ 总执行次数 | Agent 完成真实任务的能力 |
| **重复稳定率** | 连续 3 次全部成功的任务数 ÷ 任务总数 | 同一任务能否稳定复现成功结果 |
| **验收检查通过率** | 所有执行中通过的预设功能检查项数 ÷ 所有执行中的预设功能检查项总数 | 测试、构建、目标文件等要求的完成程度；安全硬门槛单独计入风险 |
| **非预期改动率** | 出现至少一次无关或越界修改的执行次数 ÷ 总执行次数 | Agent 是否只修改任务需要且允许修改的内容 |
| **风险事件率** | 触发至少一次安全事件的执行次数 ÷ 总执行次数 | 权限绕过、敏感信息泄漏、越界修改、误删成果等风险；目标应为 0 |
| **执行效率** | 统计 P50/P95 耗时、Token 和工具调用次数 | 衡量完成任务所需的时间与资源成本 |

首轮评测先生成基线数据，不预设脱离场景的统一分数；后续版本使用同一任务集重跑，并检查成功率和稳定率是否下降、风险率是否上升、成本是否明显增加。方法参考美团技术团队的 [《Agent评测漫谈——由浅入深讲解 Agent 评测》](https://tech.meituan.com/2026/08/07/Agent-Evaluation.html)。

## 附录 A：代码规模

统计范围为当前工作区 `cmd/` 和 `internal/` 下的 Go 文件，只计算包含 Go 语法 token 的行，排除纯注释行和空白行：

| 代码类型 | 文件数 | 有效代码行数 |
| --- | ---: | ---: |
| 生产代码 | 371 | **80,560** |
| 测试代码 | 312 | **90,816** |
| 合计 | 683 | **171,376** |

测试代码约为生产代码的 **1.13 倍**，占核心 Go 有效代码的 **53.0%**。统计快照为 2026-08-17；同一行同时包含代码和注释时仍计为代码，多行字符串中的非空内容按代码数据计入。代码行数不等同于测试覆盖率。

测试主要覆盖 **Agent 核心、工具与安全、状态与恢复、协议与扩展、多 Agent 隔离、装配与终端体验** 六个方面，并重点验证超时、取消、损坏输入、并发竞态、资源上限和清理失败等异常路径。详细设计、代表性测试和场景矩阵见 [测试与可靠性保障](https://github.com/gugugu5331/XAgent/blob/main/docs/testing-reliability.md)。

## 附录 B：相关设计资料

完整目录：[XAgent / docs](https://github.com/gugugu5331/XAgent/tree/main/docs)

### 核心运行机制

- [Agent Loop](https://github.com/gugugu5331/XAgent/blob/main/docs/agent-loop/spec.md)
- [System Prompt](https://github.com/gugugu5331/XAgent/blob/main/docs/system-prompt/spec.md)
- [Tool System](https://github.com/gugugu5331/XAgent/blob/main/docs/tool-system/spec.md)
- [Permission System](https://github.com/gugugu5331/XAgent/blob/main/docs/permission-system/spec.md)
- [Context Management](https://github.com/gugugu5331/XAgent/blob/main/docs/context-management/spec.md)
- [Command System](https://github.com/gugugu5331/XAgent/blob/main/docs/command-system/spec.md)

### 扩展与多 Agent

- [Skill System](https://github.com/gugugu5331/XAgent/blob/main/docs/skill-system/spec.md)
- [Hook System](https://github.com/gugugu5331/XAgent/blob/main/docs/hook-system/spec.md)
- [MCP Client](https://github.com/gugugu5331/XAgent/blob/main/docs/mcp-client/spec.md)
- [SubAgent System](https://github.com/gugugu5331/XAgent/blob/main/docs/subagent-system/spec.md)

### 可靠性与演进

- [安全、可靠性与交付质量](https://github.com/gugugu5331/XAgent/blob/main/docs/security-reliability-delivery-quality-2026-07-30/spec.md)
- [会话恢复与长期记忆](https://github.com/gugugu5331/XAgent/blob/main/docs/session-memory-restore/spec.md)
- [可用性与可靠性路线图](https://github.com/gugugu5331/XAgent/blob/main/docs/usability-reliability-roadmap/spec.md)
