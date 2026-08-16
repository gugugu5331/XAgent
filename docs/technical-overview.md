# XAgent 项目技术说明

> 用途：项目介绍、技术评审、新成员入门
>
> 基线：`codex/subagent-system`，HEAD `5418d4680ae88da76a33f2daac07f910b23bb060`
>
> 更新时间：2026-08-16（Asia/Shanghai）

XAgent 是一个使用 Go 构建的终端 AI 编程助手。它把大模型、代码工具、项目上下文、权限控制、会话恢复和子 Agent 统一到一个本地运行时中。

一句话概括：**模型负责提出意图，XAgent 负责把意图变成受控、可恢复的工程执行。**

可单独打开用于评审展示的深色图：[XAgent 架构总览](./xagent-architecture.html)。

## 1. 项目定位与价值

大模型能够理解和生成代码，但直接操作真实仓库仍存在不确定性、副作用、上下文丢失和并行冲突。XAgent 在模型与工程环境之间增加一层可治理的 Agent Runtime。

| 工程问题 | 平台能力 | 带来的价值 |
| --- | --- | --- |
| 模型输出不确定，可能调用错误工具 | 工具校验、权限确认、一次性执行凭证 | 模型不能直接绕过本地控制 |
| 文件修改和命令执行可能产生副作用 | 最小权限、路径保护、超时和输出上限 | 降低误操作和资源失控风险 |
| 长会话容易超出上下文或中断 | 上下文压缩、Artifact、会话恢复和记忆 | 支持更长、更连续的工程任务 |
| 团队流程和外部能力难复用 | Skill、Hook 和 MCP | 在不修改 Agent Loop 的情况下扩展能力 |
| 复杂任务拆解后容易互相覆盖 | 子 Agent、结果 Inbox 和 Git Worktree | 支持并行处理并保护已有成果 |

XAgent 的主要优势是：**闭环完整、安全边界前置、长任务可恢复、多 Agent 可隔离、扩展机制不侵入核心。**

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

### 3.2 复杂任务如何拆解

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

## 5. 如何保证 Agent 可靠、避免出错

可靠性不依赖模型“自觉”，而是依靠多层确定性防线：

```mermaid
%%{init: {'theme':'base','themeVariables':{'primaryColor':'#fff1f2','primaryTextColor':'#0f172a','primaryBorderColor':'#fb7185','lineColor':'#64748b','secondaryColor':'#ecfdf5','tertiaryColor':'#eff6ff','fontFamily':'PingFang SC, sans-serif'}}}%%
flowchart LR
    M["模型意图"] --> S["工具与参数校验"]
    S --> W["能力范围收窄"]
    W --> P["权限确认 + Ticket"]
    P --> E["超时 / 输出受限执行"]
    E --> F["路径与根身份保护"]
    F --> R["脱敏结果与状态记录"]
    R --> C["恢复或保守清理"]

    classDef intent fill:#fff1f2,stroke:#fb7185,color:#881337,stroke-width:2px;
    classDef guard fill:#fff7ed,stroke:#fb923c,color:#7c2d12,stroke-width:1.5px;
    classDef execute fill:#ecfdf5,stroke:#10b981,color:#064e3b,stroke-width:1.5px;
    classDef recover fill:#eff6ff,stroke:#3b82f6,color:#1e3a8a,stroke-width:1.5px;
    class M intent;
    class S,W,P guard;
    class E,F,R execute;
    class C recover;
```

| 防错机制 | 主要防止的问题 |
| --- | --- |
| 规格、计划、任务和验收清单 | 需求不清、边界遗漏、实现偏离目标 |
| Schema、Registry、Permission、Ticket | 伪造工具、参数漂移、绕过确认 |
| Workspace 最小能力 | 子 Agent 获得超出角色的工具 |
| 状态机、锁和一次性结算 | 重复完成、取消竞态、重复清理 |
| `safefs` 和 Worktree 身份记录 | 路径逃逸、符号链接风险、误删目录 |
| 超时、队列和输出上限 | 死循环、工具卡死、资源无限增长 |
| JSONL 恢复和 Artifact | 中断后数据丢失、上下文被大结果占满 |
| SafeText、SafeError 和诊断 | 敏感信息泄漏或错误无法定位 |
| 保守结算 | 状态不确定时误删用户成果 |

### 5.1 测试保障

项目同时使用单元测试、模块集成、并发与故障测试、真实 Git 场景和平台差异测试。代码规模和主要覆盖方向见附录 A，完整说明见 [测试与可靠性保障](./testing-reliability.md)。

### 5.2 信任边界

- 模型、MCP metadata、工具参数、文件路径和持久化内容默认不可信。
- Hook 是受信自动化，会从固定路径加载且不逐次确认；打开不可信项目之前必须审查。
- `tool_before allow` 不能代替后续 Permission 和 Ticket。
- Worktree 是工程隔离，不是操作系统沙箱。
- 无法证明清理安全时，系统优先保留现场和用户成果。

## 附录 A：代码规模

统计范围为当前工作区 `cmd/` 和 `internal/` 下的 Go 文件，只计算包含 Go 语法 token 的行，排除纯注释行和空白行：

| 代码类型 | 文件数 | 有效代码行数 |
| --- | ---: | ---: |
| 生产代码 | 371 | **80,560** |
| 测试代码 | 313 | **90,850** |
| 合计 | 684 | **171,410** |

测试代码约为生产代码的 **1.13 倍**，占核心 Go 有效代码的 **53.0%**。同一行同时包含代码和注释时仍计为代码，多行字符串中的非空内容按代码数据计入；代码行数不等同于测试覆盖率。

测试主要覆盖 **Agent 核心、工具与安全、状态与恢复、协议与扩展、多 Agent 隔离、装配与终端体验** 六个方面，并重点验证超时、取消、损坏输入、并发竞态、资源上限和清理失败等异常路径。详细设计、代表性测试和场景矩阵见 [测试与可靠性保障](./testing-reliability.md)。

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
