# QuantForge

量化交易研究与执行框架（Go）：回测 → 模拟盘 → 实盘 三级流水线，内置风控前置与 Kill Switch。

> 风险提示：本项目仅供学习研究。回测输出不代表实盘收益；实盘模式有真实资金风险，晋级门槛见 `docs/08-模拟盘与实盘工程.md`。

## 快速开始

```bash
# 构建单二进制（含 Web 前端）
make build

# 研究模式：启动行情 + 管理后台（无需 API Key）
cp config.example.json config.json
./bin/quantforge serve -config config.json
# 打开 http://127.0.0.1:8080

# 命令行回测（数据降级：实时 → 快照 → data/samples 固定样本）
./bin/quantforge backtest -config config.json
```

## 三级交易模式

| mode | 行为 | 前置条件 |
|---|---|---|
| `research` | 只读行情、回测、快照，不下单 | 无 |
| `paper` | OKX demo trading 模拟下单（与实盘同一套代码） | 演示环境 API Key（`OKX_API_KEY/OKX_SECRET/OKX_PASSPHRASE`） |
| `live` | 真实下单 | 配置 + 环境变量 `QUANTFORGE_ALLOW_LIVE=I_UNDERSTAND_THE_RISK` |

三种模式共用同一套策略代码、信号管道、风控规则与订单状态机，只替换交易所适配器实例——风控路径完全同构。

## 目录

```
├── cmd/quantforge/  # 入口：serve / backtest
├── config/          # 配置加载校验 + 实盘门禁
├── strategy/        # 策略接口 + 防前视 Context
├── grid/            # 网格策略（现货网格）
├── indicators/      # SMA/EMA/ATR/Donchian/波动率
├── market/          # 行情轮询 + K线质量校验 + 快照存储
├── exchange/        # 交易所抽象：okx(现货+永续) / paper(本地模拟) / binance(二期)
├── execution/       # 订单状态机 + 执行器（幂等/重试上限/先查后补/Kill Switch 检查点）
├── risk/            # 限额/敞口/频率/当日回撤停机/Kill Switch/拒单台账
├── portfolio/       # 持仓/可用/权益/对账
├── backtest/        # 事件驱动回测引擎 + 绩效指标（MDD/Sharpe/Calmar/胜率）
├── dashboard/       # Web 后台 Go API（REST + SSE）+ 内嵌前端
├── web/             # React + TypeScript + Vite 前端
├── data/            # samples 固定样本 / snapshots 快照 / backtests 结果与试验台账
└── docs/            # 知识库（方法论、工程规范、风控哲学、案例）
```

## 安全设计（写代码前先读 docs/）

- **风控前置**：所有订单先过 `risk.Manager.CheckOrder`（单笔/单日限额、敞口、频率、可卖校验、当日回撤），拒单写 JSONL 台账绝不静默。
- **Kill Switch**：一键撤单+停机；当日亏损超限自动触发；live 模式下复位必须重启进程。
- **防前视**：策略 Context 只暴露已收盘历史 K 线；回测中信号在下一根 K 线成交（禁止同根先看后成）。
- **幂等与重试纪律**：clientOrderID 去重、重试上限 3 次 + 指数退避、回报先查后补（防乌龙指式重发风暴）。
- **数据纪律**：K 线唯一键 `exchange|symbol|interval|openTime`，缺口切断、未收盘剔除、快照双写 + manifest 追溯。

## 生产实盘工程（2026-09，详见 docs/13）

针对"进程生命周期"维度的实盘就绪改造（全部有测试覆盖，`go test ./... -race` 全绿）：

- **崩溃恢复**：订单事件 JSONL 事件溯源（`data/state/orders.jsonl`），重启后重放本地状态 →
  拉交易所挂单认领孤儿单 → 账户对账，**对账不过即冻结下单**（RECONCILE_BLOCK，人工介入）。
- **账户级对账**：周期对账余额/持仓（默认 5 分钟，容差可配），差异超限自动冻结 + Telegram 告警 +
  dashboard 展示；Kill Switch 复位前置对账（对不上不许复位）。
- **权益看门狗**：独立巡检（默认 30s）当日回撤，不再依赖"有新订单才检查"；日内基线跨重启延续
  （当日已亏损不因重启被遗忘）。
- **限流与断流**：交易所 429/限流码识别 → 长退避重试（不再立即失败）；适配器内置 REST 限速器；
  行情断流连续 3 次失败（约 90s 内）即告警，失败后 30s 快重试。
- **优雅退出**：SIGTERM → 可配撤单 → 状态落盘 → 干净退出（30s 预算）；挂单留守模式由重启恢复编排认领。
- **账本口径**：手续费按 FeeCcy 分账（Base 币手续费不再错记 USDT）；费率可配置（默认 8/10 bps 对齐 OKX）。
- **策略运行态持久化**：网格 lastIdx/broke、趋势止损锚点跨重启延续；配置漂移拒绝导入（强制冷启动防错位）。
- **部署**：`deploy/quantforge.service`（systemd 单元 + 沙箱加固参考）。

接真实资金仍须走 docs/08 晋级门槛：模拟盘达标 → 风控演练 → 人工书面批准 → 小资金灰度。

## 测试

```bash
go test ./...
cd web && npm run build
```

## 运行

```bash
# 本地
go build -o quantforge ./cmd/quantforge && ./quantforge serve -config config.json

# Docker（配置放 /data/config.json，密钥走环境变量）
docker build -t quantforge .
docker run -v $PWD/data:/data -p 8080:8080 quantforge
```

环境变量：`OKX_API_KEY/OKX_SECRET/OKX_PASSPHRASE`（交易）、
`TG_BOT_TOKEN/TG_CHAT_ID`（告警）、`QUANTFORGE_ALLOW_LIVE`（实盘门禁）、
`https_proxy`（网络受限环境）。
