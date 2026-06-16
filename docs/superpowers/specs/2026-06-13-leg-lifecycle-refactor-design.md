# Leg 生命周期重构设计 — Send 做薄,职责拆模块

Date: 2026-06-13
Status: Draft for review
Branch: v2
延续/深化自: `docs/superpowers/specs/2026-06-11-send-lane-recvhandler-refactor-design.md`
相关: `docs/superpowers/specs/2026-06-11-fec-differential-leg-switching.md`(QoS,本文只提供前提,不实现)、
`docs/superpowers/specs/2026-05-08-bandwidth-probe-train-design.md`(BW gate 协议)、
`docs/superpowers/specs/2026-06-11-drr-scheduler-design.md`(DRR)

## 1. 背景与目标

`2026-06-11-send-lane-recvhandler-refactor-design.md` 定了大框架:薄 Send(只 `Write`/`WriteFrame`/
`WriteTo`/`Packets`)、`recv.Handler` 边界、per-lane FEC。v2 已落地这部分。

但老 send 仍把**五类职责**挤在一个 `*Send`(581 行 module.go + control/probe/bandwidth_probe/
rtt_state/retry/hello_timeout/leg_controller):数据通路、HELLO retry、存活探测编排、TCP fallback 拨号、
RTT/observer 编排。本文把后四类(控制平面 / leg 生命周期)从 Send 拆出,Send 只留数据通路 + lane 容器。

核心难点:一条底层链路的生命周期(dial → HELLO 握手 → 存活探测 → 死 → 重建)由谁拥有,且
**不让 leg 概念以"一条腿"复活、不让纯逻辑模块理解 send 内部寻址**。

## 2. 核心概念:`leg` 重定义

`2026-06-11` spec §10 说"不引入 public leg 模块,leg 概念吸收进 lane 内部 primary/shadow transport
policy"。本文据此**重定义 leg**:

> **`leg` 不再是"一条腿(UDP腿/TCP腿)"。它是:一条 lane 之下、管理该 lane 的底层 transports
> (UDP + TCP)状态 + transport 选择的那一层。**

- `1 lane : 1 leg`(一对一)
- leg 是 **send 内部类型,不暴露**(符合 §10)
- 对外只露 `transport.Ref`(leg 选择的输出,符合 §5.2)
- **transport.Ref 是无状态地址标识;有状态的是 leg**(它持有底层 transport 的 active/quality/dial 状态)。
  此前设计反复卡住,根因是错把无状态 Ref 当"有状态载体"——leg 才是那个有状态抽象。

层次:`lane → leg(1:1)→ 底层 UDP/TCP transports`。
- lane = 调度身份(id/weight,给 scheduler)+ per-lane FEC txWindow + leg
- leg = transport 状态管理层

leg 管:transport 状态 + 选择。leg **不管动作**(真正 Dial、发 PING/HELLO、ping 计时逻辑)——动作经注入
回调由上层提供。leg 只**消费** probe/Hello 的状态结论(死/活/握手成),更新自己的 transport 状态。

## 3. 贯穿原则

1. **纯逻辑模块零身份**:Hello / ping / bw / selector / observer / dialer 不知道 send 内部寻址。它们代码里
   **不出现** `transport.Kind` / `laneID` / `sessionID` / `leg` / `protocol.X` 任何类型。它们通过
   **无参/语义值回调**汇报(如 `OnDown()`、`OnUp()`),身份在创建者(send/runtime)的闭包里。
   - 澄清:模块"知道行为"(主动调 sendMsg 发包、调 Observer 报告),但"不知道身份"(发往哪条 leg、报给谁)。
2. **session 包零依赖**:不依赖 protocol/transport。Hello 不构帧、不持 leg;caps/fecProfile 以裸 uint16/uint8
   传递,构帧在上层闭包。
3. **Send 公开面恒为 4 个**:`Write`/`WriteFrame`/`WriteTo`/`Packets`。不新增任何 transport 状态方法。
4. **lane 生命 = session 生命**:无 `closeLane`。transport 死活在 leg 层处理(markDown/dial,lane 不动);
   session 销毁时 lanes 随 session 释放。老的 `closeLane`/`closeSessionLanes`/`closeLaneKeepingLeg`/
   `CloseScopeLane` 是错误设计(关 lane 又保留 leg 概念自相矛盾),不照搬。

## 4. 架构总览

```
runtime(胶水:懂 protocol + 懂 send 寻址 + 持 session.Manager + 注入 StreamTransport)
  ├── Recv → RecvHandler(recv.Handler)
  │     DATA/REPAIR 本地;控制帧派发:
  │     OnHello→回 HELLO_ACK 帧  OnPing→回弹 PONG 帧
  │     OnPong→laneManager.ping  OnBW_ACK→laneManager.bwLoop  OnBW_PROBE→bw.Receive 回 ACK
  └── Send(薄,4 公开方法)
        ├── lanes: lane = 调度身份(id/weight) + FEC txWindow + leg(1:1)
        │     leg(send 内部,不暴露,有状态):
        │       udp/tcp{ ref, active, quality } + primaryKind +
        │       selector(注入) + selectRef(role)→Ref
        │       markActive/markDown/bindUDP/bindTCP
        ├── DRR scheduler(吃 lanes,选 lane 承 DATA)        [既有]
        ├── bwScheduler  (吃 lanes,gate 串行带宽探测)        [新增,send 内部]
        ├── dialer per-lane(只 TCP,退避自驱,非阻塞)         [新增]
        └── OnLegFailure(实现 transport.LegFailureHandler,TCP I/O 死→leg.markDown(tcp)+dialer)
  laneManager(send 的胶水层,临时;send 与 RecvHandler 共持):
        主动 ping 实例(per legKey) + 主动 BwLoop(per trainID) + send 注册的回调闭包
        send 驱动+注册闭包,handler 喂回执 → 找同一实例;谁都不暴露方法给谁
```

## 5. 模块设计

### 5.1 Hello(`internal/session`,零依赖)

HELLO 是 session 初始化握手,`Session.Open` 返回的 `*Hello` **自治管理 retry**(spec §5.1:Session owns
HELLO open/ack/**retry** state)。

```go
type HelloConfig struct {
    Caps       uint16        // 裸数据,不引入 protocol
    FECProfile uint8
    Interval   time.Duration // 重发间隔
    Timeout    time.Duration // 放弃超时(先固定;RTT 自适应待 observer 接入)
}

// sender:怎么发 HELLO(上层闭包构帧+WriteFrame+捕获 leg;nonce 经 View 传出)
// onExpire:超时放弃后果(上层闭包:leg.markDown + 触发 dialer)
func (s *Session) Open(ctx context.Context, cfg HelloConfig,
    sender func(ctx context.Context, v View) error, onExpire func()) *Hello

func (s *Session) Ack(nonce uint64, accepted bool) bool   // 关内部 channel,停 loop
```

- Hello 内部起 goroutine loop:立即发一次 → 每 `Interval` 重发 → `Ack` 停 / `Timeout`→onExpire / `ctx.Done` 兜底。
- 删 `Hello.Retry`(现为只查 pending 的空壳)。
- 身份归属:nonce/pending/retry 定时 = Session(Hello 自治);"该 nonce 为哪条 leg 发的" = 上层
  sender/onExpire 闭包持有。Hello 不知 leg。

### 5.2 leg(`internal/tunnel/v2/send/leg.go`,内部,不暴露)

```go
type legTransport struct {
    ref     transport.Ref
    active  bool             // 默认 false;HELLO_ACK/PONG → true
    // TCP only:
    dialing bool
    backoff time.Duration
    retryAt time.Time
}
type leg struct {
    mu          sync.Mutex
    udp, tcp    legTransport
    primaryKind transport.Kind          // DATA→primary, REPAIR→shadow
    selector    selector.Selector       // 注入(transport/selector 包)
    observer    *observer.Observer      // 裁剪后的 quality 来源
}
func (g *leg) markActive(k transport.Kind)        // 收到 HELLO_ACK/PONG
func (g *leg) markDown(k transport.Kind)          // ping 超时(UDP)/ I/O error(TCP)/ hello expire
func (g *leg) bindUDP(ref transport.Ref)          // 配置注入
func (g *leg) bindTCP(ref transport.Ref)          // dial 成功后补入
func (g *leg) selectRef(role role) transport.Ref  // primary/shadow;避开 dead;零 Ref=丢包
```

**select 规则**:primary active→primary;primary 死 & shadow active→shadow;两条都死→零 Ref(Write 丢包)。
两条都活时由 selector 决策。

### 5.3 selector(`internal/transport/selector`,从老 `tunnel/send/leg/selector.go` 切)

接口不变,Quality 裁剪。

```go
type Quality struct {
    Active       bool
    DeliveryRate float64
    SmoothedRTT  time.Duration
    RTTVariance  time.Duration
    PreferTCP    bool   // 老 BandwidthPreferTCP,probeBW 冷启动锁定
}
type Selector interface { Pick(udp, tcp Quality) (useUDP, ok bool) }
```

优先级(沿用老 QualitySelector 结构,裁剪后):
```
1. 两腿都死 → ok=false(不可用)
2. 只一条活 → 那条
3. UDP 丢包高 & TCP 好 → TCP        ← 丢包形 QoS(差分质量)
4. UDP 抖动大 & TCP 好 → TCP        ← 抖动质量
5. PreferTCP → TCP                  ← probeBW 冷启动锁定
6. 默认 → UDP
```
质量层(3-4)在 PreferTCP(5)**之前**:质量信号优先,PreferTCP 是质量无异常时的冷启动兜底。未来 FEC 差分
QoS 接管 = 在 3-4 喂入差分检测结论(UDP limited 等效"UDP 丢包高"),"QoS 有数据就接管"自然成立。
删老 selector 的 `passiveUnderExplored`/passive 分支。

### 5.4 observer(`internal/transport/observer`,从老 `leg/observer.go` 切 + 裁剪)

沿用 delivery + rtt + inflight。**裁剪**:保留 `Active/DeliveryRate/SmoothedRTT/RTTVariance/PreferTCP`,
删 `BandwidthBps/ProbeLoss/ProbeSamples/BandwidthQoSLimited/Passive*` 及内部 `bandwidthState`/`passiveState`。
连带切 `rtt`(老 `send/rtt`)、`inflight`。

### 5.5 ping 扩展(`internal/tunnel/v2/probe/ping`)

在现有 Ping(只算 RTT)上**加存活判定**,不另起 liveness 模块。接口仍零身份。

```go
type Config struct {
    Interval, Timeout time.Duration
    SendMsg  func(Message) error
    MaxLoss        int          // 默认 3(沿用老值)
    RecoverSuccess int          // 沿用老值
    OnDown   func()             // 连续超时 MaxLoss → 宣布死
    OnUp     func()             // 死后连续 pong RecoverSuccess → 宣布活
    Observer func(Quality)      // RTT 样本回流(经 adapter 喂 leg.observer)
}
```
- 内部新增 lossCount/recoverCount/dead。Start tick 时:超时累计 lossCount,到 MaxLoss → OnDown();
  Pong 成功清零,若 dead 则 recoverCount++ 到 RecoverSuccess → OnUp()。

### 5.6 dialer(`internal/tunnel/v2/send/dialer.go`,per-lane,只 TCP)

```go
type dialer struct {
    remote   string
    dial     func(ctx context.Context, remote string) (transport.Ref, error) // 注入 StreamTransport.Dial
    onDialed func(ref transport.Ref)   // 成功:leg.bindTCP + 启动 TCP hello
    backoff  time.Duration             // 5→10→20→30s
}
func (d *dialer) start(ctx context.Context)   // 后台 goroutine,非阻塞
```
- 删老 `legController` 的 `udp.Active && !tcp.Active` 周期检查。

### 5.7 laneManager(`internal/tunnel/v2/send/lanemanager.go`,send 与 RecvHandler 共持)

收发共享的 probe 状态仓库。**中立**:只装实例 + 不透明闭包,不理解 lane/transport 内部。

```go
type LaneManager struct {
    mu      sync.Mutex
    pings   map[legKey]*ping.Ping   // 主动 ping;send 注册,handler 喂 PONG
    bwLoops map[uint64]*bw.BwLoop   // 主动 BwLoop by trainID;bwScheduler 存,handler 喂 ACK
}
func (m *LaneManager) RegisterPing(key legKey, p *ping.Ping)
func (m *LaneManager) LookupPing(key legKey) *ping.Ping
func (m *LaneManager) PutBwLoop(trainID uint64, l *bw.BwLoop)
func (m *LaneManager) LookupBwLoop(trainID uint64) *bw.BwLoop
```
- **临时定位**:先让收发共用状态跑通,以后想清楚再拆。
- bw 被动 Receive(passiveRound 位图)、ping 被动回弹(无状态)**不进** laneManager,归 handler 侧。

### 5.8 bwScheduler(`internal/tunnel/v2/send/bwscheduler.go`,per-session,自驱)

粗、自包含;gate 全部逻辑在内部。与 DRR scheduler 并列(都吃 lanes)。

```go
type bwScheduler struct {
    isClient  bool                    // 握手定(主动发 HELLO=client)
    lanes     func() []*lane          // 只读视图
    sendProbe func(legKey, bw.Probe)  // WriteFrame 闭包
    onSample  func(legKey, bw.Sample) // → observer(PreferTCP)
    newLoop   func(legKey) *bw.BwLoop // 创建 BwLoop 存 laneManager
    capBps    uint64
    gate      gate                    // {laneIdx, kind, phase}
}
func (s *bwScheduler) Start(ctx context.Context)        // 自驱;冷启动跑一次
func (s *bwScheduler) advanceAfterLocal(key legKey)     // 我方探完(Sample)
func (s *bwScheduler) advanceAfterRemote(key legKey)    // 收到对端探完(remaining==0)
func (s *bwScheduler) abort(key legKey)                 // leg 死→中止 train+释放 gate
```
- gate 逻辑照搬老 `bandwidth_probe.go` 的 `bandwidthProbeGate`/`advanceAfterLocal/Remote`/`nextPair`,
  改吃 v2 lane + bw.BwLoop。**send 薄**:只创建时给 isClient + lanes 视图 + 闭包,不碰 gate 逻辑。

## 6. 存活模型(UDP 与 TCP 不同)

| | UDP | TCP |
|--|-----|-----|
| active | 收到 PONG / HELLO_ACK | dial 成功 + write/read 无 error(/HELLO_ACK) |
| dead | ping 连续超时(MaxLoss=3) | **transport write/read/dial error** |
| 靠 probe 判死活? | 是 | **否,靠 I/O error** |
| 发 PING? | 发 | **也发**(为 selector 提供 RTT/quality),但存活判定不靠它 |

**两个死亡信号源,都在 send 内部落地 `leg.markDown`:**
- **UDP 死(ping 超时)**:ping 实例在 laneManager。**OnDown 闭包由 send 注册 ping 时绑定**(闭包捕获 send
  自己的 leg)。ping 触发 OnDown → 执行 send 的闭包 → `leg.markDown(udp)`。全在 send 闭包内,不暴露方法。
- **TCP 死(I/O error)**:`OnLegFailure(leg, err)` 在 send 内部实现(`transport.LegFailureHandler`;
  `stream.go` 在 write/read 出错时已调)。直接 `leg.markDown(tcp)` + 触发 dialer。不经 laneManager。

`active` 默认 false,必须对端回应过(HELLO_ACK/PONG)才置 true——非"有 ref 即活"。

## 7. 数据流

### 7.1 出站 DATA / REPAIR(既有,微调 select)
```
TUN → Write → 活动 session → DRR pickLane → lane.leg.selectRef(primary) → ref
  ref.Kind==0(两腿皆 dead)→ drop(丢包)
  else → DATA 帧 → FEC txWindow.commit → WriteTo(ref)
         窗口满/flush → REPAIR → leg.selectRef(shadow) → WriteTo
         (TCP 未就绪 → shadow 降级走 UDP,DATA/REPAIR 同腿;接收端感知单 transport,不做 QoS 差分)
```

### 7.2 HELLO 握手(Hello 自治)
```
createLane → Session.Open(ctx, cfg, sender, onExpire)
  Hello goroutine:立即发 → Interval 重发 → Ack 停 / Timeout→onExpire / ctx.Done
  对端 OnHello → Session.GetOrCreate → 回 HELLO_ACK
  本端 OnHelloAck → Session.Ack(nonce) → 闭包 leg.markActive(kind)
  onExpire 闭包:leg.markDown(kind) + 触发 dialer(TCP)
```

### 7.3 ping 存活 / RTT
```
createLane → send 创建 ping(per legKey),注册进 laneManager + 注册 OnDown 闭包
  ping.Start 自驱 tick:发 PING(sendMsg 闭包→WriteFrame)+ cleanupExpired + 数 lossCount
  对端 OnPing → 回弹 PONG
  本端 OnPong → laneManager 找同一 ping → ping.Pong → 成功:active + RTT 喂 observer;
                超时累计 MaxLoss → OnDown → 闭包 leg.markDown(udp)
```

### 7.4 TCP dial(dialer 非阻塞)
```
createLane → 立即启动 dialer(后台 goroutine);UDP 不 dial(配置注入)
  dialer:StreamTransport.Dial(remote)
    成功 → leg.bindTCP(ref) → 启动 TCP hello 握手 → active
    失败 → 退避 5→10→20→30s 重试
  重建:TCP markDown(OnLegFailure / hello onExpire)→ dialer 重启
  非阻塞:createLane 不等 dial;Write 在 TCP 未就绪时只用 UDP
```

### 7.5 bw 探测 / gate(冷启动一次)
带宽探测须打满链路才测得准,故**全局串行**:任何时刻只有一端、一条 lane、一个 kind 在探。
```
bwScheduler(吃 lanes,isClient 分支)维护 gate 队列:
  lane1-TCP → lane1-UDP → lane2-TCP → ...(capBps>0 跳过 TCP,只 UDP)
  本地探:newLoop 创建 BwLoop 存 laneManager → sendProbes(probe 闭包→WriteFrame)
          对端 OnBW_PROBE → bw.Receive 被动回 ACK
          本端 OnBW_ACK → laneManager 找同一 BwLoop → BwLoop.Ack → onSample → advanceAfterLocal
  对端探:本端 OnBW_PROBE(remaining==0)→ advanceAfterRemote
  client:phase=Local 起手,换格=收到对端探完;server:phase=Remote 起手,换格=自己探完
  超时兜底 clamp(8*SRTT,500ms,10s);leg 死→中止 train+释放 gate
  bw 失败不 mark lane down(lane 健康归 ping)
  Sample → observer(PreferTCP)
```
两端各持本地 gate(非共享),靠探测帧收发同步(齿轮咬合)。

## 8. 删除清单

老 `internal/tunnel/send/`(参考后 v2 不照搬):
- `retry.go`(helloRoute/retryOpenHELLO)→ Hello 自治
- `hello_timeout.go`(RTT 自适应 timeout)→ 先固定,observer 后接
- `leg_controller.go`(fallbackEnabled/tryStartFallback/tcpWarmWanted)→ dialer
- `control.go` 的 `closeLane`/`closeLaneKeepingLeg`/`closeSessionLanes`/`CloseScopeLane`

v2 `runtime/recvhandler.go`(纠正错位):
- 删 `pings`/`bwLoops` map、`StartPing`、`StartBandwidthProbe`、`pingSender`、`bwProbeSender`、
  `lookupPing`、`lookupBwLoop`(主动探测移到 send + laneManager)
- 保留 `OnHello`(回 ACK)、`OnPing`(回弹 PONG)、`OnBandwidthProbe`(bw.Receive 回 ACK)
- 改 `OnHelloAck`→Session.Ack + 闭包 markActive;`OnPong`→`laneManager.LookupPing(key).Pong`;
  `OnBandwidthProbeAck`→`laneManager.LookupBwLoop(id).Ack`

v2 `send/lane.go`:
- `laneRuntime`→`lane` + `leg`(leg.go);`ready()` 基于 `leg.active` 而非 ref 非空;
  `primaryTransport`/`shadowTransport` 收敛进 `leg.selectRef(role)`

## 9. 分阶段实现路线

每阶段:编译通过 + 单测绿 + 不破坏既有 v2 集成测试。阶段间严格单向依赖。

### 阶段①  Hello 做粗
- 改 `internal/session/session.go`:`Open` 新签名(ctx+cfg+sender+onExpire,返回自驱 *Hello);Hello 加
  goroutine loop + ackCh;删 `Retry`。
- 改 `v2/send/send.go` `Bootstrap`:改用新 `Open`,sender 闭包构 HELLO 帧 + WriteFrame。
- 改 `v2/runtime/recvhandler.go` `OnHelloAck`:Session.Ack(markActive 留 stub,阶段②补)。
- 验证:session 单测(无 Ack 按 Interval 重发计数 / Timeout 触发 onExpire 一次 / Ack 立即停);
  `v2/integration_test.go` 的 TestHELLOThroughSession / TestHELLOACKValidatesNonceViaSession 改签名后仍绿。
- 依赖:无(打底)。

### 阶段②  leg + selector + observer + dialer
- 新 `internal/transport/selector/`(从老 `leg/selector.go` 切,裁剪 Quality,迁移 selector_test)。
- 新 `internal/transport/observer/`(从老 `leg/observer.go` 切,裁剪 bandwidth/passive;连带 rtt、inflight,
  迁移 observer_test、rtt_test)。
- 新 `v2/send/leg.go`(leg + legTransport + markActive/markDown/bind*/selectRef)。
- 新 `v2/send/dialer.go`(per-lane TCP dialer,退避,注入 Dial)。
- 改 `v2/send/lane.go`(laneRuntime→lane+leg;ready 基于 active;DATA/REPAIR 走 selectRef)。
- 改 `v2/send/send.go`(createLane 立 lane+leg+启动 dialer+bindUDP;sendDataFrame/sendRepair 用 selectRef,
  零 Ref→drop;Config 加 StreamTransport;实现 `transport.LegFailureHandler.OnLegFailure`→leg.markDown(tcp)+dialer)。
- 接线:onExpire→leg.markDown+dialer;HELLO_ACK→leg.markActive。
- 验证:selector/observer/leg/dialer 单测;OnLegFailure→markDown→dialer 重启;select 矩阵(单活/双死→零Ref)。
- 依赖:①。

### 阶段③  probe 存活 + laneManager
- 改 `v2/probe/ping/ping.go`:加 lossCount/recoverCount/dead + Config 的 MaxLoss/RecoverSuccess/OnDown/OnUp/Observer。
- 新 `v2/send/lanemanager.go`(RegisterPing/LookupPing/PutBwLoop/LookupBwLoop)。
- 改 `v2/send/send.go`:createLane 创建 ping → 注册进 laneManager + 绑 OnDown 闭包(捕获 leg)。
- 改 `v2/runtime/recvhandler.go`:删主动探测;OnPong→laneManager.LookupPing(key).Pong;
  laneManager 由 runtime 与 send 共持(构造时注入)。
- 验证:ping 存活单测(连续超时 MaxLoss→OnDown 一次;恢复→OnUp);端到端 PING/PONG round-trip 经 laneManager;
  OnDown→leg.markDown 串通。
- 依赖:②(OnDown→leg.markDown)。

### 阶段④  bw + bwScheduler + gate
- 改 `v2/probe/bw/bw.go`:拆主动(BwLoop)/被动(`Receive.Probe(p) (Ack, bool)`,返回 Ack 让 handler 发)。
- 新 `v2/send/bwscheduler.go`(gate 串行,isClient,照搬老 gate 逻辑改吃 v2 lane + BwLoop)。
- 改 `v2/runtime/recvhandler.go`:OnBandwidthProbe→bw.Receive 回 ACK;OnBandwidthProbeAck→
  laneManager.LookupBwLoop(id).Ack;remaining==0→通知 bwScheduler.advanceAfterRemote。
- 改 `v2/send/send.go`:创建 bwScheduler(注入 isClient/lanes/闭包),冷启动 Start。
- 验证:bwScheduler gate 单测(client/server 错拍逐拍核对、换格时机、超时兜底);被动 Receive 回 ACK 单测;
  bw 失败不 markDown lane。
- 依赖:③(laneManager 装 BwLoop)。最独立,放最后。

## 10. 非目标

- 不实现 FEC 差分 QoS 决策(`2026-06-11-fec-differential-leg-switching.md` 的职责);本文只提供前提
  (per-lane 状态、preferTCP 钩子、REPAIR 走 shadow、单 transport 降级感知)。
- 不引入 public leg 模块;leg 是 send 内部类型。
- laneManager 是临时胶水层,不是最终的收发分层方案。
- 不改全局 `internal/transport` 包的对外类型(`transport.Ref` 仍以 LegRef 别名表达)。

## 11. 测试策略

| 层 | 检查 |
|---|---|
| session/Hello | Open 自驱重发计数;Timeout→onExpire;Ack 停 loop;零依赖(不 import protocol/transport) |
| selector | 优先级矩阵:双死/单活/UDP丢包/UDP抖动/PreferTCP/默认;裁剪后无 passive |
| observer | RTT 估算;DeliveryRate;裁剪后无 bandwidth/passive 字段 |
| leg | markActive/markDown × select 矩阵;双死→零 Ref;active 默认 false |
| ping | 连续超时 MaxLoss→OnDown 一次;恢复→OnUp;零身份(无 transport.Kind 等) |
| dialer | fake Dial 失败→退避序列;成功→onDialed 一次;非阻塞 |
| OnLegFailure | TCP I/O error→leg.markDown(tcp)→dialer 重启 |
| laneManager | send 注册 + handler LookupPing/LookupBwLoop 找同一实例 |
| bwScheduler | client/server gate 逐拍;换格时机;超时兜底;leg 死释放 gate;bw 失败不 markDown lane |
| 端到端 | Send.Write→Recv FEC 恢复(已有);PING/PONG round-trip;dial 后两腿常驻 |
| 边界 | probe/ping、probe/bw、session 无 multipath transport/protocol/send import |
