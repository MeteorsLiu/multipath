# Leg 质量测量与切换重构：FEC 异路测量 + LINK_REPORT 回流 + 角色状态机

日期：2026-06-10
状态：设计完成，待评审
分支：v2

## 1. 问题背景

当前 leg selector 的目标是 UDP 遭 QoS 时自动切到 TCP、TCP 质量差时回到 UDP，但两个方向都失效。根因（按代码定位）：

1. **带宽探测一次性**：`bandwidthLegState.complete` 置位后永不复位（`bandwidth_probe.go`），`qosLimited`/`preferTCP` 只反映会话建立头几十秒的测量。运营商 QoS 通常在流量跑起来之后才生效，那时已无探测在跑。
2. **ping 丢失不计入 delivery rate**：超时 ping 在 `inflightTracker.pruneTarget` 中被静默删除。丢包越狠，delivery rate 反而越"干净"。（已于本次前置修复：超时计为失败样本。）
3. **delivery tracker 终身累计**：好历史稀释当前劣化。（已前置修复：32 样本滑动窗口 + 8 样本最小门槛。）
4. **`preferTCP` 是无条件锁存**：`selector.go` 中 `if udp.BandwidthPreferTCP { return false, true }` 不看 TCP 自身质量，且无解除路径——切到 TCP 后 UDP 无流量，passive 探索排在锁存之后永远轮不到，死锁。
5. **规则不对称**：所有切换规则都是"UDP 坏→TCP"，没有"TCP 坏且 UDP 恢复→UDP"。

更深层的设计问题：现架构把"是否被 QoS"当**分类问题**（bool 锁存 + 阈值），分类需要锁存、锁存需要解除条件、解除需要探测、探测又太贵。本设计把问题改回**估计问题**：持续估计每条 leg 此刻的送达能力（连续量），决策跟着估计值走。

### 根本性约束

发送端对"对面收到了什么"物理性失明。任何基于送达率的方案（TCP ACK、QUIC ACK、RTCP RR）都必须有接收端→发送端的信息回流。本设计的回流通道是 PONG 捎带的 LinkReport。

### ping 探测的局限（为什么必须用真实大包测量）

QoS 典型形态是掐大包/大流量（DPI 标记、令牌桶限速），放过小包。ping 是小包低频（200ms 一个），只能证明"链路通"，证明不了"业务流量没被掐"。测量必须用与业务包同等待遇的流量——MTU 级、速率正比于业务——repair 包正好满足。

## 2. 设计总览

```
┌─ 角色模型 ──────────────────────────────────────────────┐
│ primary leg：承载 100% 业务 DATA                         │
│ shadow  leg：承载 100% FEC REPAIR（顺便成为持续探测流）  │
└──────────────────────────────────────────────────────────┘
              │ 发送端 per-leg×类别 累计 sent 计数
              ▼
┌─ 测量回流 ─────────────────────────────────────────────┐
│ 接收端 per-leg×类别 累计 recv 计数，每秒快照            │
│ 快照捎带于 PONG/PING（caps 协商，reportSeq 去重）       │
└──────────────────────────────────────────────────────────┘
              │ 发送端差分相邻快照
              ▼
┌─ 估计与决策 ───────────────────────────────────────────┐
│ per-leg 丢包率 = 1 − recvΔ/sentΔ → EWMA(τ≈2s)           │
│ 角色状态机：STEADY →(原子切换+驻留)→ STEADY ⇄ PROBING    │
└──────────────────────────────────────────────────────────┘
```

稳态业务流量 100:0（非 bonding，已确认）；灰度比例仅出现在回切试探与切换过渡期。

### 信号源三层降档

| 条件 | 信号源 | 角色 |
|---|---|---|
| 有业务流量 | 计数差分（DATA 行测 primary，REPAIR 行测 shadow） | 权威 |
| 接近空闲（窗口样本不足） | ping delivery rate（32 样本窗口，已实现） | 兜底 |
| 冷启动 | probeBW 测参考带宽 | 一次性 |

FEC 恢复事件计数（`recover_emit`）**不进决策**，仅保留为 metrics 观测。原因：repair 异路提前到达会触发"超前恢复"（TCP 未丢但慢，UDP 路 repair 先到），使恢复计数虚高，不纯粹等于 primary 丢包数。差分计数无此偏差。

## 3. 角色模型与 FEC 异路

### 3.1 规则

- 每条 lane 维护一个角色指派：`primaryKind ∈ {UDP, TCP}`，初始 UDP（仅 UDP active 时 UDP；仅 TCP active 时 TCP）。
- DATA 帧全部走 primary；REPAIR 帧全部走 shadow。shadow 不存在（单 leg lane）时 repair 回落 primary，行为同现状。
- 角色互换时 repair 流向自动反转（恒为"非 primary"），无需额外逻辑。

### 3.2 为什么 repair 走 shadow 是安全甚至更优的

逐角色组合论证（关键设计依据，评审重点）：

**UDP=primary, TCP=shadow（常态）**：repair 走 TCP。TCP 可靠传输，repair 100% 到达（至多迟到）。UDP 丢数据包时 repair 从 TCP 稳定到达，FEC 恢复照常。优于现状：现状 repair 与数据同走 UDP，丢包相关——UDP 丢 30% 时 repair 也丢 30%，FEC 最需要时最弱。异路天然去相关。

**TCP=primary, UDP=shadow（切换后）**：repair 走 UDP 可能被 QoS 丢——无害，因为 primary 是 TCP，本身不丢包，FEC 此方向无恢复任务。repair 在 UDP 上纯当测量流，丢失正是要测的信号。

结论：**无需"劣化回拉"逻辑**（早期讨论曾设想 primary 劣化时把 repair 拉回原路双发，推演后确认不需要，已删除）。

### 3.3 重复包安全性（已验证现有代码覆盖）

repair 异路引入"repair 先到→超前恢复→原包后到"序列。现有去重机制完整拦截：

- `rxSLCWindow.emitted` 表（`rx_window.go:192`）：每 PacketID 仅放行一次，数据路径（`recv.go:264` → `data_drop_duplicate`）与恢复路径（`recv.go:413` → `recover_drop duplicate`）共用。
- 反序（原包先到、repair 后到）：group 凑齐时 repair 直接丢弃（`rx_window.go:132` all_known），不触发恢复。

已知边界（接受，不加固）：`emitted` 随 data 窗口 FIFO 淘汰（`maxData=4096`）。灰度期高吞吐（>40k pps 单路）且副本迟到 >100ms 时去重可能漏。后果良性：上送的是内层 IP 包，偶发重复在 IP 交付语义内（内层 TCP 按 seq 去重，UDP 应用本须容忍）。监控手段：`data_drop_duplicate` 日志。若实际观察到问题，后续可将 emitted 换为"基准 ID+位图"（64k ID 仅 8KB）扩深 16 倍。

超前恢复是收益：TCP 重传卡住的包被 UDP 路 repair 提前救出，降低尾延迟。

## 4. 协议变更

### 4.1 caps 协商

```go
CapLinkReport uint16 = 1 << 2          // protocol/body.go
SupportedCaps = CapTCPFallback | CapFEC | CapLinkReport
```

HELLO/HELLO_ACK 协商机制复用现状。两端均带 `CapLinkReport` 时启用 PONG/PING 尾部扩展；否则帧格式与行为完全同现状（向后兼容）。

### 4.2 LinkReport 块（20 字节，追加于 PingBody 尾部）

```go
type LinkReport struct {
    ReportSeq     uint32  // 快照序号，单调递增；发送端按此去重与断流检测
    UDPDataRecv   uint32  // 本端从 UDP leg 累计收到的 DATA 帧数
    UDPRepairRecv uint32  // 同上，REPAIR 帧
    TCPDataRecv   uint32  // 本端从 TCP leg 累计收到的 DATA 帧数
    TCPRepairRecv uint32  // 同上，REPAIR 帧
}
```

编码：PingBody 现为 16 字节（PingID+TimeMS）。caps 含 LinkReport 时 PING/PONG body 为 36 字节，定长追加；解码端按协商 caps + body 长度判断是否携带。无 TLV，YAGNI。

设计决策记录：

- **uint32 计数**：差分用模 2³² 无符号减法，回绕自动正确（同 TCP seq）。不需要 64 位。
- **累计值而非窗口值**（核心决策）：报告丢失无害——下一份报告差分自动跨更宽窗口，计数永远自洽，无需重传/对齐/丢失补偿。RTCP RR 同款思路。
- **快照每秒滚动，pong 每 200ms 重复携带**：接收端每秒拍快照、ReportSeq++；其间 5 个 PONG 携带同一份。统计成本固定每秒一次，热路径仅 memcpy 20B。同份快照经 2 leg × 5 pong 冗余送达。
- **PING 同样捎带**：两端互 ping，多一次免费送达机会，接收侧按 seq 去重。
- **方向对称**：A→B 流量由 B 计数、随 B 发出的 PONG/PING 流回 A、驱动 A 的发送决策。反方向镜像。两端同代码。单向 QoS 下出现"A→B 走 TCP、B→A 走 UDP"是正确行为。
- 发送端 sent 计数不进报告：发送端自有（`OnSent` 钩子扩展为 per-leg×类别）。报告只补发送端看不见的 recv 半边。

### 4.3 接收端计数点

`recv` 模块帧分发处，按帧来源 leg 的 Kind 与帧类型递增计数器。计数 DATA 与 REPAIR 的**到达帧数**（去重前——测的是链路送达，不是应用接受）。

## 5. 差分与估计算法

### 5.1 差分

计数器是二维的：`[leg ∈ {UDP,TCP}] × [class ∈ {DATA,REPAIR}]` 共 4 个，发送端、接收端各一套（LinkReport §4.2 的 4 个字段就是接收端这 4 格）。

发送端每收到 ReportSeq 更新的报告，对每一格独立做差分：

```
对每个 (leg, class) 格:
  sentΔ(leg,class) = sent_now(leg,class) − sent_prev(leg,class)     // 发送端本地快照对
  recvΔ(leg,class) = report(leg,class) − prev_report(leg,class)     // 模 2^32
  lossRate(leg,class) = 1 − recvΔ(leg,class) / sentΔ(leg,class)
```

状态机消费其中两格（由当前角色指派决定取哪两格）：

```
p = lossRate(primary leg, DATA)      // primary 丢包率：业务数据全在 primary，DATA 格样本最密
s = lossRate(shadow leg, REPAIR)     // shadow 丢包率：repair 全在 shadow，是 shadow 唯一的流量

例：UDP=primary 时  p = lossRate(UDP, DATA)，s = lossRate(TCP, REPAIR)
    角色互换后      p = lossRate(TCP, DATA)，s = lossRate(UDP, REPAIR)
```

其余两格的 sentΔ 在稳态下为 0（DATA 不走 shadow、REPAIR 不走 primary），无定义也无消费者；灰度期 `lossRate(原primary, DATA)` 格重新有流量，PROBING 状态直接用它判档位成败（§6.2）。

发送端须在收到报告时同步快照自己的 sent 计数，与 ReportSeq 配对存储（仅保留上一对，O(1) 内存）。

### 5.2 数值示例（含 QoS 触发全过程）

场景：业务 2000 pps 走 UDP(primary)，repair 500 pps 走 TCP(shadow)，RTT 50ms。T=10.0s 起 UDP 遭 QoS 丢 30%（TCP 不受影响）。

```
发送端本地（每秒快照）:               接收端快照（随 pong 回流）:
T=10.0  sent(UDP,DATA)=100000         seq=50 @T≈10.0  recv(UDP,DATA)= 99900  recv(TCP,REPAIR)=24975
T=11.0  sent(UDP,DATA)=102000         seq=51 @T≈11.0  recv(UDP,DATA)=101300  recv(TCP,REPAIR)=25475
T=12.0  sent(UDP,DATA)=104000         seq=52 @T≈12.0  recv(UDP,DATA)=102700  recv(TCP,REPAIR)=25975
        sent(TCP,REPAIR)=25000/25500/26000（每秒+500）

窗口 50→51:
  p = lossRate(UDP,DATA)   = 1 − (101300−99900)/(102000−100000) = 1 − 1400/2000 = 30%
  s = lossRate(TCP,REPAIR) = 1 − (25475−24975)/(25500−25000)    = 1 −  500/500  = 0%

EWMA(α=0.39): P: 0 → 11.7% → 18.8% → 23.2% → ...    S 恒 ≈ 0%
T≈11s P 越过 5% 且 S<1%（双条件成立），劣化计时器启动；每窗口 +1s；T≈16s 满 5s → 触发切换
QoS 生效到切换完成 ≈ 6s（< 10s 目标）
```

### 5.3 精度分析

**在途包误差**：窗口两端各有 ~rate×RTT 在途量，差分相减时互相抵消；仅速率突变的单个窗口有瞬时误差，量级 RTT/窗口 = 50ms/1s = 5%，经 EWMA 摊薄不足以穿越确认窗口。报告中不需要附加锚点字段（最大 PacketID 等留作未来精化，本版不做）。

**报告丢失**：seq=51 丢失时下次差分 50→52 自动跨 2s 窗口，结果仍正确。

**样本量与可信度**（统计基础，决定最小样本门槛）：二项噪声 σ=√(p(1−p)/n)。5s 确认窗口内：

| 业务流量 | 测 primary 的样本(DATA) | 噪声±1σ | 5% 阈值判定 |
|---|---|---|---|
| 2000 pps | 10000 | ±0.2% | 可靠 |
| 200 pps | 1000 | ±0.7% | 可靠 |
| 40 pps | 200 | ±1.5% | 边缘 |
| ~空闲 | ~0 | — | 不可判定 |

**最小样本门槛 200**：确认窗口内样本不足 200 则不判定，窗口自动延展至凑够（累计差分天然支持）。低流量判断慢是接受的取舍（低流量时用户对劣化不敏感）。接近空闲降档至 ping delivery rate。

### 5.4 EWMA

每报告窗口更新一次：`P += α·(p−P)`。τ≈2s、窗口 1s 时 α=1−e^(−1/2)≈0.39。窗口因报告丢失变宽时按窗口时长折算 α（α=1−e^(−Δt/τ)）。

## 6. 角色状态机（per lane，发送端）

### 6.1 参数表（10s 检测目标定调，全部为常量，首版不做配置项）

| 参数 | 值 | 说明 |
|---|---|---|
| reportInterval | 1s | 接收端快照周期 |
| ewmaTau | 2s | 丢包率 EWMA 时间常数 |
| degradeEnter | 5% | 劣化计时累计阈值（Schmitt 上沿） |
| degradeExit | 3% | 劣化计时清零阈值（Schmitt 下沿；3%~5% 之间计时冻结） |
| shadowHealthy | 1% | 切换要求 shadow 丢包率低于此值 |
| confirmWindow | 5s | 劣化持续确认时长 |
| minSamples | 200 | 确认窗口内最小样本数，不足则窗口自动延展 |
| dwellTime | 30s | 切换后最短驻留，期间不再切换 |
| recoverObserve | 10s | shadow 持续健康观察期，满后启动灰度回切 |
| probeSteps | 10%→50%→100% | 灰度档位（DATA 按比例分流回原 primary） |
| probeStepDwell | 5s/档 | 每档驻留与观察 |
| reportStale | 3s | ReportSeq 停滞超时，按 primary 最坏情况处理 |

### 6.2 状态与迁移

记号：**P** = primary leg 丢包率的 EWMA 估计（由 DATA 行差分驱动）；**S** = shadow leg 丢包率的 EWMA 估计（由 REPAIR 行差分驱动）。两者绑定角色而非 leg 种类，角色互换后测量来源随之互换。

```
STEADY（常态）
  每新报告: 差分→EWMA 更新 P/S
  if P>degradeEnter 且 S<shadowHealthy 且样本≥minSamples:
      confirmTimer += 窗口时长
      if confirmTimer ≥ confirmWindow → 执行切换（原子动作，见下）
  elif P<degradeExit: confirmTimer=0
  if ReportSeq 停滞≥reportStale → 视同 P=最坏 → 执行切换
  if 在驻留期后 S 持续<shadowHealthy 达 recoverObserve 且原 primary 为偏好 leg(UDP)
      → PROBING

切换（原子动作，非状态）
  角色互换标志位翻转：下一个 DATA 走新 primary，repair 流向随动反转
  P/S 估计值随角色对调（新 primary 的估计延续自旧 S），confirmTimer=0
  进入 dwellTime（期间禁止再次切换）

PROBING（灰度回切试探，仅 TCP=primary 恢复 UDP 方向）
  档位 i ∈ {10%, 50%, 100%}: DATA 按比例分流至 UDP，驻留 probeStepDwell
  期间 UDP DATA 行差分丢包率 > degradeEnter → 立即全部退回 TCP，
      重置 recoverObserve 观察期（含指数退避：10s→20s→40s，封顶 5min）
  100% 档驻留满 → 角色互换（UDP 复位为 primary），进入 dwellTime → STEADY
```

要点与依据：

- **双条件切换**（P 坏且 S 好）：两路同坏（整网故障）时切换无收益，原地不动靠 FEC 与上层重传扛。
- **Schmitt 双阈值**：单阈值在 4.9%↔5.1% 抖动时计时器被反复清零，切换饿死。双阈值保证进入劣化区后轻微回落不重置进度。
- **切换是原子动作而非过渡状态**：隧道是数据报模型，每包独立选 leg，无任何需要拆建的关联——"切换"只是标志位翻转，不存在 break，无需 make-before-break。切换瞬间旧 leg 上最后 ~RTT 在途包的丢失风险由 FEC 覆盖（方案 B 下 repair 此刻正在健康的新 primary 上 100% 到达，这恰是 FEC 的本职场景）。早期设计中的 500ms DATA 双发过渡经推演删除：双发期短于 EWMA 时间常数，起不到验证作用，纯冗余。
- **灰度回切是对"纯限速型 QoS"的充分验证**：shadow 上 repair 流量小（~业务 1/4），令牌桶型限速可能放过它——shadow 健康只是必要条件（流未被标记），不是充分条件（容量可达）。灰度 10% 档若真有限速，秒级现形且只伤 10% 流量，TCP 仍承载主体，FEC 兜底，用户无感。
- **PROBING 仅回 UDP 方向**：UDP 是偏好 leg（无队头阻塞）。TCP=shadow 劣化驱动的反向切换走 STEADY 的正常判定（此时 primary=UDP 的劣化由 DATA 差分测出）。

### 6.3 与现有调度的接合

`writeScheduledFrame`（send.go:137）的 `legSelector.Pick(udpQ, tcpQ)` 替换为查询 lane 的角色状态机：返回 primary leg（PROBING 期按档位比例分流）。`Selector` 接口保留，`QualitySelector` 实现改读角色；`UDPPrefersSelector` 不变（不协商 LinkReport 时的行为=现状，见 §8 兼容性）。REPAIR 路径（`maybeSendRepair`）不再走 `writeScheduledFrame` 的 leg 选择，改为显式取 shadow leg 入队。

## 7. 旧机制处置（已确认决策）

| 对象 | 处置 |
|---|---|
| `bandwidthState.qosLimited`/`preferTCP` 及 `updateQoS` | 删除 |
| selector 的 `BandwidthPreferTCP` 规则、passive 探索规则（`passiveUnderExplored`） | 删除 |
| 两个未实现的 TDD 测试（passive bytes 均衡 / passive 带宽比较） | 删除 |
| probeBW 体系 | 保留，仅冷启动测参考带宽；周期性 QoS 判定职责移除 |
| `Quality` 中 passive 带宽字段 | 保留（probeBW cap 参考仍用），探索决策用途移除 |
| ping delivery rate（32 样本窗口） | 保留为低流量兜底信号 |

## 8. 兼容性与降级

- **caps 未协商成功**（对端旧版本）：不附加 LinkReport，repair 仍走 primary（同现状），角色状态机不启用。selector 退化为保留规则集：ping delivery rate（32 样本窗口）+ RTT 抖动规则（§7 删除的仅是 bandwidth/passive 规则，delivery/jitter 规则保留）。即新机制整体 feature-gated by caps。
- **单 leg lane**：状态机静止于 STEADY，repair 回落 primary。
- **报告断流**（reportStale）：按 primary 最坏处理触发切换；若两 leg 报告均断（反向全断）切换也无意义，但切换无害（驻留期防振荡）。
- **杀掉 shadow 的 QoS 不影响业务**：shadow 只有 repair，丢了不伤 FEC 保护主体（见 §3.2）。

## 9. 实现组件清单

| 组件 | 位置 | 内容 |
|---|---|---|
| 协议扩展 | `internal/protocol/body.go` | CapLinkReport、PingBody 尾部 20B 编解码（按 caps+长度） |
| 接收端计数器 | `internal/tunnel/recv` | per-leg×类别累计计数、每秒快照、ReportSeq |
| 报告附带 | send/control.go PING/PONG 收发路径 | 出站捎带最新快照；入站提取报告上抛 |
| 发送计数 | `leg.Observer.OnSent` 扩展 | per-leg×类别累计；收到报告时快照配对 |
| 差分估计器 | `internal/tunnel/send/leg`（新文件） | 差分、EWMA、样本门槛、断流检测 |
| 角色状态机 | `internal/tunnel/send/leg`（新文件） | §6 状态机，驱动自报告事件 |
| 调度接合 | send.go / 选择器 | Pick 改读角色；repair 显式走 shadow；灰度档位分流 |
| 旧逻辑删除 | observer.go / selector.go / bandwidth_probe.go | §7 清单 |
| metrics | metrics 包 | per-leg 丢包率、角色、状态机迁移事件、灰度档位 |

## 10. 测试策略

**单元**：

- 差分：常规窗口、报告丢失跨窗口、uint32 回绕、ReportSeq 乱序/重复去重、断流检测
- EWMA：时间常数、变宽窗口的 α 折算
- 状态机：全部迁移路径、Schmitt 行为（4.9↔5.1 抖动不饿死、不误切）、双条件（两路同坏不切）、驻留期内禁切、灰度各档失败回退与指数退避
- 协议：caps 协商矩阵（新↔新、新↔旧、旧↔旧）、PingBody 带/不带报告的编解码往返

**集成**（复用现有 e2e 测试设施）：

- UDP 单向丢包 30% → 10s 内角色互换，业务不中断
- QoS 解除 → recoverObserve + 灰度三档 → UDP 复位 primary
- 灰度期 UDP 仍限速 → 第一档退回，退避生效
- TCP=primary 劣化、UDP 健康 → 反向切换
- repair 异路 + TCP 延迟 → 超前恢复发生且无重复上送（断言 data_drop_duplicate 路径）
- 接近空闲流量 → 不误切（样本门槛生效）

**回归**：FEC 恢复率在 repair 异路下不低于同路（相关丢包场景下应更高）；现有 send/recv 全量测试通过。

## 11. 已知边界与未来工作

- 高吞吐灰度期去重窗口溢出（§3.3，良性，监控 data_drop_duplicate）
- 在途误差精化锚点（报告附最大 PacketID）——本版不做
- bonding（常态按权重分流聚合带宽）——明确不在本版范围
- 检测延迟 10s 为保守定调，参数表集中一处，未来可按场景调整或做成配置
