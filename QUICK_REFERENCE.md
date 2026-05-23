# MATRIX ASSET ENGINE v71 — 快速参考卡片

## 🚀 快速开始

```bash
# 1. 初始化（首次运行）
go run . -init

# 2. 编辑输入文件
vim matrix_operator_input.json

# 3. 运行引擎
go run .

# 4. 查看结果
cat matrix_persistent_state.json
```

---

## 📋 命令行参数

| 参数 | 说明 | 默认值 |
|------|------|--------|
| `-init` | 创建默认输入文件并退出 | - |
| `-input` | 指定输入文件路径 | `./matrix_operator_input.json` |
| `-state` | 指定状态文件路径 | `./matrix_persistent_state.json` |

---

## 📄 输入文件格式

**文件名**：`matrix_operator_input.json`

```json
{
  "System_Awake_Date": "2026-05-23",
  "Target_Inflow_RMB": 7000.00,
  "SGOV_Balance_RMB": 0.00,
  "W_current": {
    "SMH": 0.0,
    "QQQM": 0.0,
    "ORBX": 0.0,
    "URA": 0.0
  },
  "W_target": {
    "SMH": 0.30,
    "QQQM": 0.40,
    "ORBX": 0.15,
    "URA": 0.15
  },
  "Heaviside_Floor_RMB": 150.0
}
```

---

## 📊 输出文件格式

**文件名**：`matrix_persistent_state.json`

```json
{
  "last_run_date": "2026-05-22",
  "sgov_balance_rmb": 0.0,
  "current_weights": {
    "SMH": 0.0,
    "QQQM": 0.0,
    "ORBX": 0.0,
    "URA": 0.0
  },
  "execution_log": [
    {
      "date": "2026-05-22",
      "delta_macro": 1.1732,
      "market_regime": "STRUCTURAL BULL TREND",
      "w_sgov_percent": 0.0,
      "phi_percent": 0.0,
      "total_bullet": 7000.0,
      "sgov_balance": 0.0,
      "allocations": {
        "SMH": 2100,
        "QQQM": 2800,
        "ORBX": 1050,
        "URA": 1050
      }
    }
  ]
}
```

---

## 🔑 核心算法速查

### Module A: SGOV 蓄水池控制阀

```
δ_macro = QQQM_spot / EMA_200d

if δ_macro >= 1.0:
    W_SGOV = 0%
else:
    W_SGOV = clip(1.5 × (1.0 - δ_macro), 0%, 60%)
```

### Module B: SGOV 出水阀（60% 安全垫）

```
if δ_macro < 1.0 AND S_deficit > 0.05:
    φ = clip(1.2 × S_deficit, 0%, 40%)
else:
    φ = 0%

bucketInjection = SGOV_Balance × φ
```

### Module C: 香农自适应纠偏

```
d_i = W_target[i] - W_current[i]
S_deficit = Σ max(0, d_i)

if S_deficit > 0:
    w*_i = max(0, d_i) / S_deficit
else:
    w*_i = W_target[i]
```

---

## 🎯 市场状态判定

| δ_macro | 市场状态 | SGOV 蓄水 | SGOV 出水 |
|---------|----------|-----------|-----------|
| ≥ 1.0 | 牛市通道 | 0% | 0% |
| < 1.0 | 股灾左侧 | 最高 60% | 最高 40% |

---

## 📈 资产配置目标

| 资产 | 战略权重 | 说明 |
|------|----------|------|
| QQQM | 40% | 全球科技/核心大盘底盘 |
| SMH | 30% | 全球半导体/算力核心体系 |
| ORBX | 15% | 卫星航天/SpaceX 概念奇点 |
| URA | 15% | 铀矿/下一代代际能源硬通货 |
| SGOV | 动态 | 物理现金缓冲水桶 |

---

## ⚠️ 常见问题

### Q1: 输入文件不存在
```bash
[FATAL] Operator input protocol error: failed to read operator input file
[HINT] Run with -init flag to create default input file
```
**解决**：`go run . -init`

### Q2: 日期格式错误
```bash
[FATAL] Invalid System_Awake_Date: parsing time "..." as "2006-01-02"
```
**解决**：确保格式为 `YYYY-MM-DD`（如 `2026-05-23`）

### Q3: ORBX 数据不足
```bash
[WARNING] ORBX continuity check failed: ORBX has no data, using ITA synthetic proxy
```
**说明**：正常降级行为，系统自动使用 ITA 代理

### Q4: Yahoo Finance 限流
```bash
[FATAL] QQQM MARKET DATA ERROR: HTTP status 429
```
**解决**：等待几分钟后重试

---

## 🔄 每月运行流程

```bash
# Step 1: 更新输入文件
vim matrix_operator_input.json
# 修改：Target_Inflow_RMB（当月注资）
# 修改：SGOV_Balance_RMB（上月水桶余额）
# 修改：W_current（当前真实权重）

# Step 2: 运行引擎
go run .

# Step 3: 根据终端输出执行交易
# 查看 "## ASSET | ROUTE_WEIGHT % | FIAT_ALLOCATION" 部分

# Step 4: 记录本次运行结果
# 系统已自动保存到 matrix_persistent_state.json
```

---

## 📁 文件清单

| 文件 | 类型 | 说明 |
|------|------|------|
| `main.go` | 源码 | 主控制流（6 个 Phase） |
| `input_protocol.go` | 源码 | 输入协议与持久化状态 |
| `trading_calendar.go` | 源码 | 美股交易日历 |
| `matrix_operator_input.json` | 配置 | 操作员输入文件（运行时） |
| `matrix_persistent_state.json` | 状态 | 持久化状态文件（运行时） |
| `test_system.sh` | 测试 | 集成测试脚本 |
| `README_v71_PRODUCTION.md` | 文档 | 完整生产文档 |
| `DIAGNOSTIC_REPORT_FINAL.md` | 文档 | 最终诊断报告 |
| `QUICK_REFERENCE.md` | 文档 | 本快速参考 |

---

## 🧪 测试命令

```bash
# 运行完整集成测试
./test_system.sh

# 验证输入文件格式
jq empty matrix_operator_input.json

# 验证状态文件格式
jq . matrix_persistent_state.json

# 查看执行历史
jq '.execution_log' matrix_persistent_state.json
```

---

## 🔐 安全检查清单

- [ ] 输入文件中的日期格式正确（YYYY-MM-DD）
- [ ] 所有金额字段为非负数
- [ ] 目标权重总和为 1.0
- [ ] 当前权重总和 ≤ 1.0
- [ ] Heaviside 阈值 > 0
- [ ] 网络连接正常（Yahoo Finance 可访问）
- [ ] 磁盘空间充足（至少 10MB）

---

## 📞 支持

如有问题，请查阅：
1. `README_v71_PRODUCTION.md`（完整文档）
2. `DIAGNOSTIC_REPORT_FINAL.md`（诊断报告）
3. 联系首席量化架构师

---

**MATRIX ASSET ENGINE v71_SHANNON_LEVIATHAN_FLUID**  
**STATUS: 🟢 GREEN — PRODUCTION READY**  
**Last Updated: 2026-05-23**
