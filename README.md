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

## 测试

```bash
go test ./...
cd web && npm run build
```
