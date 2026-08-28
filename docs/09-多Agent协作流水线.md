# 多 Agent 协作流水线

四角色子 Agent（`.claude/agents/`）把"情报 → 分析 → 风控 → 执行"拆成权责隔离的四段，每段只做一件事、只写自己的输出目录、只消费上一段的产物。设计动机与知识库总纲一致：**可审计（每步留痕）、权责分离（分析者不控仓位，风控不下单，交易员无观点）、风控一票否决（任何信号不过 Kriss 不得执行）**。

## 一、角色与流水线

```
Charles（信息搜集员）      Zoe（数据分析师）         Kriss（风控官）          Ethan（交易员）
八卦 / 敏感 / 快           严谨 / 数学好 / 死板       悲观 / 胆小              冷酷 / 算法专家
─────────────────  ─────────────────────  ─────────────────────  ─────────────────────
抓 RSS/公告/搜索     读简报→拉K线→规则卡打分    七条否决+半凯利仓位链     只执行批准指令
      │                      │                       │                       │
      ▼                      ▼                       ▼                       ▼
data/intel/briefs/    data/agents/signals/     data/agents/reviews/     data/agents/executions/
YYYY-MM-DD.{md,json}  YYYY-MM-DD-<sym>.md      YYYY-MM-DD-<sym>.md      YYYY-MM-DD-<sym>.md
      └──── 简报 ────→┴──── 信号 ─────────────→┴──── 决议 ─────────────→┴──── 执行单
```

| 角色 | 定义文件 | 只做 | 永不做 |
|---|---|---|---|
| Charles | `.claude/agents/charles.md` | 搜集、溯源、分级情报 | 给买卖建议、编造新闻、预测涨跌 |
| Zoe | `.claude/agents/zoe.md` | 按规则卡打分、产出枚举信号 | 声明胜率/上涨概率、决定仓位 |
| Kriss | `.claude/agents/kriss.md` | 否决清单 + 半凯利仓位链、终审决议 | 下单、改阈值、改 Zoe 信号 |
| Ethan | `.claude/agents/ethan.md` | TWAP 拆单执行单 + 对账留痕 | 接受无决议指令、绕风控路径、超仓位执行 |

## 二、交接契约（每段的输入只认上游产物文件）

1. **简报 → 信号**：Zoe 只读 `briefs/*.json`，按 importance 加权换算情报因子；无简报则情报因子记 0，不得凭记忆补。
2. **信号 → 决议**：Kriss 只审 `signals/*.md` 中的 schema JSON；关键 reviewFlags 一票否决；决议三态 `APPROVE / SCALE_DOWN / REJECT`，附 `targetPositionPct` 与 `kellyChain` 全程可复算。
3. **决议 → 执行**：Ethan 只认 `reviews/*.md` 的 `APPROVE/SCALE_DOWN`；`REJECT` 或无决议 → `NOT_EXECUTED` 并留痕。

## 三、对剧本的两处必要修正（人设有趣，纪律优先）

四角色的初始剧本里有两处与知识库冲突，落地版已修正：

| 剧本原话 | 问题 | 修正后 |
|---|---|---|
| Zoe："胜率预测 65%" | `confidence` 不是上涨概率（SKILL.md 表述红线）；胜率只能由历史回测统计 | Zoe 输出 confidence(0–100) 表示推理确信强度；历史胜率由 Kriss 从回测台账统计，用于凯利公式且必须打半折 |
| Kriss："总仓位已达 80%…建议只买 1%" | 80% 仓位本身已违反"永不满仓" | 总仓位 ≥80% 是 Kriss 的一票否决项（连 1% 都不买，先减仓再说）；现金比例永远 ≥20% |

## 四、知识库映射（每个角色开工会先读哪些笔记）

| 角色 | 必读 | 关键条款 |
|---|---|---|
| Charles | docs/01（可见时间对齐）、docs/02（三层数据）、docs/03（不编造事实） | published_at 用源的发布时间；无 URL 不可溯源标 `RUMOR_UNVERIFIED`；空简报好过假简报 |
| Zoe | docs/03（信号 schema）、docs/01（假设卡/防前视）、SKILL.md | 七枚举 + confidence/score 分职；只用已收盘 K 线；数字不可追溯即阻断 |
| Kriss | docs/04（半凯利/回撤优先）、docs/08（晋级矩阵）、SKILL.md | f\*×0.5（弱信号 ×0.3）→ 波动率打折 → 单标的 ≤10% → 现金 ≥20%；live 需人工书面批准 |
| Ethan | docs/05（滑点/拆单）、docs/08（订单状态机/重试）、SKILL.md 五不 | 只限价单；TWAP 分片带幂等 ID；禁市价单；先查单后补单；熔断即停 |

## 五、触发方式

**手动逐角色**：依次对主 agent 说"跑 Charles / 跑 Zoe（基于今日简报）/ 跑 Kriss（审 BTC 信号）/ 跑 Ethan（执行）"。Claude Code 会自动匹配 `.claude/agents/` 定义；ZCode/Codex 等环境由主 agent 以通用 subagent 启动、把对应定义文件全文作为任务说明传入（定义自包含，无隐式依赖）。

**每日 08:00 定时**（Charles 的简报节奏），系统 cron 示例：

```cron
0 8 * * * cd /path/to/QuantForge && claude -p "$(cat .claude/agents/charles.md) 请生成今日简报" >> data/logs/charles_cron.log 2>&1
```

后续 Zoe→Kriss→Ethan 由人看过简报后手动触发（"人看过再往下走"本身就是流程，不是待优化项：docs/01 的权责四类主体中，人是唯一决策主体）。

## 六、审计与留痕

- 四段产物各占一个目录（`briefs / signals / reviews / executions`），文件名带日期与标的，任何一单可逆向追溯：执行单 → 决议 → 信号 → 简报 → 源 URL。
- 全链 JSON 字段对齐 docs/03 schema 与五级失败阻断；任一角色自检不过 → 重跑该角色，禁止手改产物文件"修复"数据。
- 失败也要留痕：`NOT_EXECUTED`、空简报、`INSUFFICIENT_DATA` 都是合法终态，禁止删除或美化。

## 七、边界与演进

- 当前版本**不改 Go 代码**：Ethan 的执行能力受现有 CLI 限制（`serve` 为策略自动交易，无手动下单子命令），live 执行单由人触发。若后续在 Go 侧新增 `pipeline`/`twap` 命令（`agents/` 包 + `execution` 之上加 TWAP 调度器），本流水线的契约可直接映射为代码接口：Intel → Signal(risk 前置) → Decision(半凯利) → TWAP 执行单。
- 情报源（`data/intel/sources.json`）只用无需凭据的公开源；接入 X/Twitter 等需凭据源时，密钥走环境变量，不入库不进定义文件（docs/08 密钥纪律）。
