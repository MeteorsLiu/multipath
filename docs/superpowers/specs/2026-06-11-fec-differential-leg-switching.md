# FEC 差分：leg 质量测量与切换（重设计）

日期：2026-06-11
状态：设计完成，待评审
分支：v2
取代：`2026-06-10-leg-quality-switching-design.md`（整体废弃；仅 primary/shadow 角色模型及其安全性论证沿用，见 §4）

## 1. 出发点

- RTT 不是问题：per-leg ping/pong 已持续测量。
- 测不出的是两腿**送达带宽的差距**，且必须低成本：
  - 主动探测（probeBW）成本高，**保留但只跑一次**（冷启动）——有些网络从第一天就掐 UDP，由 probeBW 定初始 primary；`bandwidthLegState.complete` 的一次性行为在本设计中是意图而非缺陷，动态变化全部归差分管。
  - 纯被动观测有自我强化死锁：被选中的腿才有流量，未选中的腿永远没有样本。
- 破局点：FEC repair 恒为业务的固定比例，让它走另一条腿，未选中腿上**永远有一条与业务成正比、与业务同等待遇的真实测量流**。
- 拓扑前提：所有 lane 恒有 UDP/TCP 两腿，不存在单腿情形。

## 2. 设计内核：FEC 组是自描述的探测单元，接收端全知

每个 FEC 组是一次对照实验——同一时刻、同一批数据，4 个包走 primary、1 个 repair 走 shadow。接收端**仅凭协议常识**即可知道"该到什么"，发送端无需报数：

- **该收多少 DATA**：PacketID 水位线。双保险——数据腿丢成筛子也无妨，shadow 上的 repair 携带 basePacketID+span，照样暴露发送端进度；
- **该收多少 repair**：组数（每 4 个 ID 一组），数据与 repair 互相证明对方存在；全组覆灭由水位线兜出；
- **何时到**：同组 repair 与数据末包的到达时差（lag），repair 从健康腿先到，给同组数据当秒表。

三个差分量覆盖三种劣化形态：

| 差分量 | 来源 | 测出什么 |
|---|---|---|
| 丢包差分 | 水位线该到 vs 实到，按腿记账 | UDP 被掐（丢大包型 QoS） |
| 时延差分 | repair 进度参照 vs 数据腿送达水位 | TCP 被限速（不丢包、只积压） |
| 容量实测/下界 | 每腿实收字节 ÷ 窗口 | 见 §3 |

**带宽测量原理（核心）**：被打饱和的腿，送达速率＝真实容量。被限到 2 Mbps 的腿，灌多少到达端都是 2 Mbps——与 probeBW 同数同精度，区别仅是 probeBW 制造饱和、这里利用业务的真实饱和。"腿劣化"恰恰是"腿饱和"，**需要测带宽的时刻正好就是能免费测到带宽的时刻**。唯一测不出的是"未饱和腿超出当前流量的上限"，用 §3 的下界 + 可逆试探绕开，每次试探失败还白得一个实测值。

## 3. repair 与业务带宽的关系

代码依据（`internal/fec/fec.go:50-55`）：repair 长度 = 组内 4 个 DATA 的**最大值**（短包零填充参与 GF(256) 线性组合）。

- 按包数：repair 恒为 DATA 的 1/4；
- 按字节：repair/DATA = max/sum ∈ **[25%, ~100%]**。max ≥ 均值是恒等式，故 **repair 字节 ≥ DATA 字节 25% 永远成立**；包大小越不均匀占比越高（如 1×1400B+3×60B 的组，repair 占 89%）。
- 推论一：shadow 容量下界用**实测** repair 字节速率，25% 仅是最坏情况保底，不进任何计算；
- 推论二：repair 尺寸恒等于组内最大包——测量流天生全部由"大包"构成，与 QoS 最爱掐的尺寸等级同待遇，结构性避免"业务大包被掐、探测小包通过"的假阴性（ping delivery rate 效果差的根源之一）。

## 4. 角色模型（沿用原设计，唯一保留物）

- 每条 lane 一个 `primaryKind ∈ {UDP, TCP}`，初始由冷启动 probeBW 决定（无结果时 UDP）。DATA 100% 走 primary，REPAIR 100% 走 shadow；
- 角色互换时 repair 流向自动反转（恒为"非 primary"），无额外逻辑；
- 安全性论证（沿用原 §3.2/§3.3）：
  - UDP=primary, TCP=shadow：repair 经 TCP 必达，UDP 丢数据时 FEC 最需要时最强，异路天然去相关；
  - TCP=primary, UDP=shadow：repair 被丢无害（TCP 不丢包，FEC 无恢复任务），丢失正是要测的信号；同时 repair 是 TCP 积压检测的进度参照物（§2），这是角色模型在接收端推断里的承重作用；
  - 重复包/超前恢复：`emitted` 表已覆盖（数据/恢复双路径共用，rx_window.go），超前恢复是收益；高吞吐去重窗口边界沿用原结论（良性，监控 `data_drop_duplicate`）。

## 5. 接收端：组账本与判定

检测全部下放接收端（它本就全知），挂在 rx window 旁，按方向独立、两端同代码：

**记账**（滑动窗口 W≈3s）：

- `dataExpected`：水位线差（取数据腿 ID 与 repair basePacketID+span 的较大者；小幅重排宽限）；
- `dataGot`/`repairGot` 与对应字节数：按**到达腿 × 帧类别**，**去重前**计数（链路送达语义）；
- `repairExpected`：窗口内组数；span 以 repair 头部为准，flush 小组天然兼容；
- `lag`：每组 t(数据末 shard) − t(repair)，窗口中位数，带符号；开放组上限（~2s/256 组），超限按"≥上限"记；
- 切换瞬间跨腿组的归因噪声至多一组/次，接受。

**判定**（沿用阈值，迁移至接收端）：

```
limited(leg)    = 丢包率 > 5%，持续 sustain(3s)        // 进入；低于 1% 解除（迟滞）
backlogged(leg) = lag > 基线 + 300ms，持续 sustain      // 基线 = 长窗滚动最小 lag
实测带宽(leg)   = 该腿窗口实收字节 ÷ W                  // 随状态一并上报
样本门槛        = 窗口 dataExpected < 100 → 不判定（静默，发端 ping 兜底接管）
```

## 6. 反馈：LINK_STATUS（电平触发 + 刷新 + TTL）

周期性上报不存在。反馈只在异常时发生：

```
LINK_STATUS 帧（~8B）：{legKind u8, reason u8 (limited|backlogged), deliveredBps u32}
```

- **电平触发**：异常持续期间每 ~1s 重发，两条腿都发。单发一次是边沿触发，恰好诞生在链路出事的时刻，丢了就永远丢了——必须刷新；
- 发送端持有 per-leg 状态，**TTL 3s**：刷新续期，刷新停止自动过期。恢复检测免费：QoS 解除 → 接收端停止刷新 → 状态过期 → 偏好规则拉回，无需 CLEAR 包、无需 ACK；
- 一切健康 → 全程静默，零字节开销；
- caps 协商：`CapLinkStatus`。未协商（对端旧版）→ 整套机制关闭（含 repair 异路路由），行为=现状。

## 7. 发送端：选腿规则

检测在接收端，决策留在发送端（它持有 hold/退避上下文，也是执行者）：

```
两腿均无 limited/backlogged 状态 → 偏好 UDP
仅一腿异常                      → 走另一条（切换原子：primaryKind 取反，repair 随动）
两腿均异常                      → 比较状态包携带的实测 bps，走数字大的那条
切换后 hold(10s) 内禁再切
回偏好：UDP 状态过期后须再静默 preferWait(10s) 才切回；
        切回后 sustain 内再收到 UDP 异常 → 切走，preferWait 翻倍（封顶 5min）；
        存活 60s → preferWait 复位
近空闲（接收端静默且无业务流量）→ 现有 ping 规则兜底（delivery 32 样本窗 + RTT），
        闲置腿死亡由此路径切换
冷启动 → probeBW 一次，定初始 primary
```

注：旧设计"两腿同坏不切"在此升级为"择优"——状态包带实测 bps，2 Mbps 与 8 Mbps 都受限时去 8 那边。

## 8. 参数表（常量，首版不做配置）

| 参数 | 值 | 位置 | 说明 |
|---|---|---|---|
| W | 3s | 接收端 | 滑动窗口 |
| lossEnter / lossExit | 5% / 1% | 接收端 | limited 进入/解除（迟滞） |
| lagSlack | 300ms | 接收端 | 时延差分容差（基线之上） |
| sustain | 3s | 接收端 | 判定持续确认 |
| refresh / TTL | 1s / 3s | 收/发 | LINK_STATUS 刷新与过期 |
| hold | 10s | 发送端 | 切换后最短驻留 |
| preferWait | 10s，×2 封顶 5min，存活 60s 复位 | 发送端 | 回偏好观察与退避 |
| sampleFloor | 100 | 接收端 | 窗口最小样本 |

## 9. 场景走查（业务 20 Mbps，2000 pps）

| 场景 | 差分表现 | 行为 |
|---|---|---|
| UDP(primary) 被限到 2 Mbps | UDP 该到 7.5MB/3s 实到 0.75MB → 实测 2 Mbps、丢包 70%；TCP repair 全到 → ≥5 Mbps | 接收端刷 UDP limited(2Mbps) → 发送端切 TCP（检测+通知 ≈ 6s，<10s） |
| QoS 解除 | UDP repair 转干净 → 刷新停止 | TTL 过期 + preferWait 后切回 UDP |
| 温和限速（UDP 实际 3 Mbps，>下界） | 切回后数秒 UDP limited(3Mbps) 再现 | 切回 TCP，preferWait 翻倍；3 Mbps 实测值入账 |
| TCP(primary) 被限到 8 Mbps | 不丢包；UDP repair 进度到 16000、TCP 送达 10000 → 积压，lag 秒级 | 接收端刷 TCP backlogged(8Mbps) → 切 UDP |
| 两腿同时受限 | 双状态各带实测 bps | 择优走大的 |
| 接近空闲 | 无组可判，接收端静默 | ping 兜底 |
| FEC=Off | 无 repair 流，差分关闭 | ping 兜底——测量与 FEC 同生死，明示接受 |

## 10. 旧机制处置

| 对象 | 处置 |
|---|---|
| `bandwidthState.qosLimited`/`preferTCP` 锁存及 `updateQoS` | 删除 |
| selector 的 `BandwidthPreferTCP` 规则、passive 探索（`passiveUnderExplored`） | 删除 |
| probeBW 体系 | 保留，仅冷启动一次，定初始 primary |
| ping delivery rate（32 样本窗口） | 保留，近空闲兜底的真实消费者 |
| FEC | 一字不动：hardcode 4+1，flush 例外照旧；本方案对 FEC 的全部要求是"repair 换条腿发" |

## 11. 实现组件

| 组件 | 位置 | 内容 |
|---|---|---|
| repair 路由 | send `maybeSendRepair` | 显式取 shadow 入队；不得阻塞 primary 发送路径 |
| 组账本 + 判定 | recv（rx window 旁） | §5 记账、迟滞判定、LINK_STATUS 发出与刷新 |
| 协议 | `internal/protocol` | `CapLinkStatus` + LINK_STATUS 帧编解码 |
| 选腿 | send/leg | §7 规则替换现有质量规则；ping 规则保留为兜底与未协商回退 |
| metrics | metrics 包 | 每腿实测 bps/丢包/lag、角色、切换与回偏好事件、退避档位 |

## 12. 测试策略

**单元**：账本（水位线双保险、重排宽限、全组覆灭、变 span、lag 符号与开放组上限）；判定迟滞（5%↔1%、4.9↔5.1 不抖）；LINK_STATUS 编解码与 caps 矩阵；发送端 TTL 过期、择优、hold、preferWait 退避与复位、样本门槛。

**集成（e2e）**：

- UDP 单向限速/丢 30% → <10s 切 TCP，业务不中断；
- TCP=primary 限速（限速率而非注入丢包）→ backlogged 触发切回 UDP——本场景在丢包率类设计下不可构造，本版可构造；
- QoS 解除 → TTL+preferWait 后回 UDP；温和限速 → 回切失败、退避翻倍；
- LINK_STATUS 丢包注入 → 刷新机制下行为不变；
- 两腿同限 → 择优；近空闲腿死亡 → ping 兜底切换；
- repair 异路 + TCP 延迟 → 超前恢复且无重复上送。

**回归**：repair 异路下 FEC 恢复率不低于同路（相关丢包场景应更高）；send/recv 全量通过。

## 13. 边界、诚实声明与开放项

- **下界语义**：shadow 健康只证明 ≥ 实测 repair 速率（最坏 0.25R），是必要条件非充分条件。误切由可逆性自愈（数秒次优，FEC 兜底），反复误切由 preferWait 退避压制，且每次失败试探都产出一个实测带宽值。
- **lag 基线**为估计值（长窗滚动最小值 + 300ms 容差），实现期可调，参数集中一处。
- **阈值住在接收端**：两端同代码，调参需两端同步升级。开放替代：HELLO 下发阈值（本版不做）。
- **遥测**：事件化后发送端平时无连续带宽数据；如观测需要，可加纯 metrics 低频统计包（~10s，决策无关），可选。
- bonding（权重分流聚合带宽）明确不在范围。
