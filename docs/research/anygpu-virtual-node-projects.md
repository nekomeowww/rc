# AnyGPU Virtual Node 现有项目与实现路线调查

> 这是一份独立 AG（AnyGPU）项目的设计输入，目前暂存在 rc 仓库中；AG 建立独立 repository 后应迁移过去。rc 只是潜在消费者，不是 AG 的第一方模型或实现依赖。

调查日期：2026-09-09

## 结论

有现成基础，也有非常接近 AnyGPU 的先例，但没有一个项目可以原样解决问题。

建议 AG v1 采用以下组合：

1. **保留原生 `core/v1 Pod` 作为 northbound API**，用 `RuntimeClass`、taint/toleration 和可选的 scheduler profile 选择 AG。
2. **直接消费 Virtual Kubelet Go library**，复用 Pod reconciliation、Node heartbeat/Lease、Pod status 和 kubelet HTTPS streaming routes；不要 fork Virtual Kubelet，也不要从头实现 kubelet API。
3. **一个稳定的 Virtual Node 对应一个 `AGResourcePool`**，例如 provider、region、GPU family、purchase model、storage domain 和 capability 相同的一组容量。v1 不为每条报价或每个 Pod 动态创建 Node。
4. **Pod 绑定之后创建 `AGAllocation`**，由异步 controller/driver 购买 RunPod、Vast.ai 等资源。Virtual Kubelet 的 `CreatePod` 只落盘 intent 并快速返回，不能同步等待数分钟的实例启动。
5. **scheduler plugin 只读取本地 inventory snapshot 做 `Filter`/`Score`**；不在调度周期里请求 provider，不在 `Reserve`/`PreBind` 里购买实例。
6. **AG 自己拥有 AGPI、采购幂等、capability validation、数据面和 orphan GC**。Virtual Kubelet 是 Kubernetes-facing runtime library，不是 provider plugin ABI。
7. **提供 InterLink compatibility driver** 是合理的，它可以接入其 SLURM、HTCondor、remote Kubernetes 等生态；但 AG 核心不应依赖 InterLink，因为当前 plugin contract 只有 create/delete/status/logs，且项目明确偏向 batch workload。

最接近 AnyGPU 的历史项目是 **Elotl Kip**：一个稳定 Virtual Node 暴露容量，每个绑定 Pod 启动一台 right-sized cloud VM，结束后销毁。最成熟的在维护实现是 **Azure ACI Virtual Kubelet provider**：一个稳定 pool node，每个 Pod 对应一个 ACI Container Group。两者都证明了 pool-node 模型可行，也共同暴露出相同问题：Node capacity 是 broker 承诺而不是某台真实主机，PVC、网络和完整 PodSpec 语义不会因为套上 Node 自动成立。[Kip README](https://github.com/elotl/kip/blob/master/README.md)、[Azure ACI provider](https://github.com/virtual-kubelet/azure-aci/tree/v1.6.4)

## 推荐架构

```text
Deployment / Job / StatefulSet / arbitrary controller
                         │
                    core/v1 Pod
                         │
      admission: RuntimeClass + defaults + validation
                         │
             anygpu-scheduler（可选 profile）
        Filter / Score against cached pool snapshots
                         │ bind
           stable AGResourcePool Virtual Node
                         │
            ag-node (Virtual Kubelet library)
                         │ Ensure intent
                    AGAllocation
                         │
              asynchronous provisioner
                         │ AGPI
        RunPod / Vast / Modal / AutoDL / 共绩 / InterLink
                         │
                     ag-agent
```

### v1 的 Kubernetes 对象

```text
AGProviderClass
    认证引用、driver endpoint、全局 provider defaults

AGResourcePool
    稳定的 provider/region/GPU/storage/capability 调度类别
    1:1 投影成 core/v1 Node

AGInventorySnapshot
    driver 周期性生成的 offer、价格、quota、shape、freshness 快照
    scheduler 只读，不在热路径查询厂商 API

AGAllocation
    Pod UID 对应的一次外部执行租约和 operation journal
    保存 create intent、provider request/resource ID、重试与释放状态

AGPlacementPolicy
    provider allowlist、价格上限、spot、地域、trust tier 等策略
```

其中 `AGAllocation` 是内部持久状态，不是替代 Pod 的 workload API：

```text
Pod UID 1 ── 1 AGAllocation ── 1 provider workload/instance
```

### 一个 Node 表示什么

v1 中，一个 Node 表示稳定的、同质的资源池，不代表一台物理机，也不代表一条瞬时 marketplace offer：

```text
ag-vast-us-rtx4090-spot
ag-runpod-eu-a100-secure
ag-modal-us-a100-sandbox
```

Node label 只发布调度期间相对稳定并且能兑现的性质：

```yaml
metadata:
  labels:
    anygpu.io/runtime: "true"
    anygpu.io/pool: vast-us-rtx4090-spot
    anygpu.io/provider: vast
    anygpu.io/gpu-family: rtx-4090
    anygpu.io/purchase-model: spot
    anygpu.io/storage-domain: s3-us
    topology.kubernetes.io/region: us
    node.kubernetes.io/exclude-from-external-load-balancers: "true"
spec:
  taints:
    - key: anygpu.io/runtime
      value: external
      effect: NoSchedule
status:
  allocatable:
    nvidia.com/gpu: "8"
    cpu: "128"
    memory: 512Gi
    pods: "8"
```

这里的 `allocatable` 是 AG 愿意同时承诺的 admission budget，由账户 quota、费用预算、并发限制和近期库存共同限制；它不是网页当前搜到的所有 GPU，也不能像 ACI 示例那样盲目填写几千个 Pod 的“近似无限容量”。ACI provider 的默认值就是 10,000 CPU、4 TiB memory、5,000 Pods，并把实时 quota 查询留为未启用逻辑，这正说明 pool node capacity 是 policy，而非物理事实。[ACI Node capacity source](https://github.com/virtual-kubelet/azure-aci/blob/v1.6.4/pkg/provider/vk_node.go#L124-L155)

`AGResourcePool` 应足够同质，使一个已绑定 Pod 可以在池内更换报价或重新采购而不改变用户声明的硬约束。不要把不同 region、GPU family、storage topology 或安全等级混进同一 Node。

### 为什么 v1 仍需要 scheduler plugin

默认 kube-scheduler 可以根据 Node capacity、selector、affinity、taint 和 topology 完成基础调度，所以最小原型不必有 plugin。但生产版的 pool node 有两个默认 scheduler 看不到的问题：

- 聚合 capacity 足够，不代表存在一台 provider instance 能装下整个 Pod；
- 实时价格、库存 freshness、provider capability、账户预算和数据 locality 不是普通 Node resource。

因此建议提供独立 scheduler profile，扩展点只做纯读取计算：

| 扩展点 | AG 职责 |
|---|---|
| `PreFilter` | 把 PodSpec 编译成单实例 shape、GPU、数据、网络和 capability 要求 |
| `Filter` | 用同一 generation 的本地 snapshot 检查是否存在可兑现 offer，以及硬价格/策略限制 |
| `PreScore` | 固定本轮 snapshot generation，避免逐 Node 看到不同市场状态 |
| `Score` | 比较预计总价、启动 P95、抢占风险、可靠性和数据 locality |
| `Reserve` | 只暂扣 scheduler 进程内的 pool budget |
| `Unreserve` | 幂等释放本地暂扣；不能调用 provider delete |
| `PreBind` | 最多写入轻量 placement hint；不能创建计费资源或等待数据搬运 |

厂商 API 查询由 inventory controller 完成；付费资源创建由 allocation controller 完成。调度快照过期或 offer 突然消失时，Pod 已绑定但保持 `Pending/AGCapacityUnavailable`，controller 在同一 pool 内重新选择；不能把这种外部事务塞进 scheduler 的瞬时调度周期。

### Pod 生命周期

1. 用户或任意上层 controller 创建普通 Pod，声明 `runtimeClassName: anygpu`；可由 admission 注入 `schedulerName: anygpu-scheduler`、selector 和 toleration。
2. validation webhook 根据所有可选 pool 的公共 capability 做静态检查。按照当前产品策略，无法落实的 `securityContext` 字段产生 admission warning 和 Event，但不阻止创建；会导致错误数据或错误路径的 volume/volumeMount 直接拒绝。
3. scheduler 从 Virtual Node 和 `AGInventorySnapshot` cache 选择一个 pool node并绑定。
4. `ag-node` 的 Virtual Kubelet Pod controller 收到 Pod。`CreatePod` 幂等创建或确认 `AGAllocation`，然后快速返回；它不直接阻塞调用 provider。
5. allocation controller 调用 AGPI driver，按 `Pod UID + spec digest` adopt 或创建资源，准备镜像、Secret/ConfigMap 和数据。
6. driver watch 或轮询 provider；`PodNotifier` 把 `AGSelecting`、`AGProvisioning`、`AGStagingData`、`Running`、`Succeeded`、`Failed` 映射到标准 Pod/Container status。
7. `kubectl logs/exec/attach/port-forward` 进入 `ag-node` 的 kubelet HTTPS server，再由 Pod UID 查 `AGAllocation`，转发到 provider native API 或 `ag-agent` 反向通道。
8. 删除时先停止 workload、collect/checkpoint 数据并归档日志，再销毁计费资源；确认 provider absent 后才把所有 container 标为 terminal 并完成 Pod 删除。外部删除必须允许重复调用。

Pod 一旦绑定便不能改绑到另一个 Node。池内兼容实例消失时，AG 可以保持同一 Pod 并重新采购；必须跨 pool/provider 时，需要删除并由 Job、ReplicaSet 等上层 controller 重建 Pod。AG 不能假装 kube-scheduler 可以迁移一个已绑定 Pod。

## Virtual Kubelet 能直接复用什么

### 当前状态与边界

[Virtual Kubelet](https://github.com/virtual-kubelet/virtual-kubelet) 是一个用于构建自定义 Kubernetes node agent 的 Go library，而不是完整运行时或 federation solution。项目仍活跃，`v1.14.0` 于 2026-09-07 发布并升级到 Kubernetes 1.36/Go 1.26；许可证为 Apache-2.0。[v1.14.0 release](https://github.com/virtual-kubelet/virtual-kubelet/releases/tag/v1.14.0)、[LICENSE](https://github.com/virtual-kubelet/virtual-kubelet/blob/v1.14.0/LICENSE)

它提供三个关键层次：

1. `PodLifecycleHandler`：`CreatePod`、`UpdatePod`、`DeletePod`、`GetPod`、`GetPodStatus`、`GetPods`。其契约明确允许同一 Pod 多次调用 `DeletePod`，并要求 provider 最终通知 terminal status。[接口源码](https://github.com/virtual-kubelet/virtual-kubelet/blob/v1.14.0/node/podcontroller.go#L41-L76)
2. 可选 `PodNotifier`：provider 主动推送 Pod status；未实现时由 PodController 定期轮询。[接口源码](https://github.com/virtual-kubelet/virtual-kubelet/blob/v1.14.0/node/podcontroller.go#L78-L90)
3. `NodeProvider`：`Ping` 和 `NotifyNodeStatus`，由 NodeController 负责注册 Node、维持 status 和 Lease。[接口源码](https://github.com/virtual-kubelet/virtual-kubelet/blob/v1.14.0/node/node.go#L52-L71)

高层 `nodeutil.Provider` 还包括 logs、exec、attach、stats、resource metrics 和 port-forward。[Provider source](https://github.com/virtual-kubelet/virtual-kubelet/blob/v1.14.0/node/nodeutil/provider.go#L16-L43) 它的 HTTP helper 已实现 kubelet 兼容的 `/pods`、`/containerLogs`、`/exec`、`/attach`、`/portForward`、`/stats/summary` 和 resource metrics 路由，是 AG 不应自行重写的高价值协议 glue。[HTTP server source](https://github.com/virtual-kubelet/virtual-kubelet/blob/v1.14.0/node/api/server.go#L36-L100)

Pod controller 还处理 informer/workqueue、Create-vs-Update、status propagation、dangling provider Pods 和部分 downward API/env resolution。上游的 provider 接入准则特别要求 provider 本身不要直接访问 Kubernetes API，而应通过明确回调取得 Secret/ConfigMap；这与 out-of-process AGPI 的边界一致：`ag-node` 读取并解析 Kubernetes 对象，vendor driver 只收到 resolved input，不持有 kubeconfig。[Provider requirements](https://github.com/virtual-kubelet/virtual-kubelet/blob/v1.14.0/README.md#providers)、[Pod sync source](https://github.com/virtual-kubelet/virtual-kubelet/blob/v1.14.0/node/pod.go#L62-L115)

### stock Virtual Kubelet 是否支持动态多个 Node

**不直接支持。**准确说法是：底层 library 可以被重新组合，但现成的 `nodeutil`/CLI 运行模型是一进程、一个静态 Node。

- `NodeController` 的源码注释明确写着管理单个 Node entity。[NodeController](https://github.com/virtual-kubelet/virtual-kubelet/blob/v1.14.0/node/node.go#L208-L217)
- `nodeutil.NewNode` 构造一个 NodeController、一个 PodController 和按一个 `nodeName` 过滤的 Pod informer。[nodeutil controller](https://github.com/virtual-kubelet/virtual-kubelet/blob/v1.14.0/node/nodeutil/controller.go#L300-L452)
- stock CLI 接收一个 `--nodename` 并只调用一次 `NewNode`。[CLI root](https://github.com/virtual-kubelet/virtual-kubelet/blob/v1.14.0/cmd/virtual-kubelet/internal/commands/root/root.go#L82-L160)

因此不能在同一个 listener 上简单循环调用 `nodeutil.NewNode` 来动态增加 Node。若未来 AG 要支持 per-allocation Virtual Node，需要自己写 `AGNodeManager`：共享 Kubernetes informers 和一个 kubelet HTTPS server，动态管理每个 Node 的 registration/status/Lease，并按 `pod.spec.nodeName` 或 Pod UID 把请求路由到 allocation。可以继续复用 VK 的 lower-level `node` 与 `node/api` 包，但多节点生命周期、共享 listener、证书和路由都属于 AG 自己的代码。

v1 选择“一个 `ag-node` Deployment/进程对应一个稳定 `AGResourcePool` Node”可以直接沿用上游的成熟形状，先避免这部分复杂度。pool 数量增大后，再把多个稳定 Node 合并进自研的 shared manager；这与“每个 Pod 一个动态 Node”是两项不同演进，不要同时做。

### AG 应复用与自行实现的边界

| 直接复用 Virtual Kubelet | AG 自己实现 |
|---|---|
| Pod informer/workqueue reconciliation | AGPI 版本化、driver discovery/capability |
| Node registration、status、Lease | inventory/价格快照与 pool admission budget |
| Pod status 写回和 `PodNotifier` glue | `AGAllocation` journal、adopt、重试、orphan GC |
| Secret/ConfigMap/env/downward API resolution 基础 | 完整 PodSpec capability validation 与 diagnostics |
| kubelet logs/exec/attach/port-forward HTTP routes | provider native/`ag-agent` streaming backend |
| metrics/stats route glue | 数据 stage/collect、provider volumes、checkpoint |
| errdefs 与 controller 重试基础 | scheduler plugin 与 placement policy |

不要实现完整 kubelet 或 CRI。AG 的 backend 并不是本机 container runtime；照搬 kubelet 的 volume manager、device manager、CNI/CRI 调用栈既重又无法调用 SaaS provider。Virtual Kubelet 已经切出了正确的 kubelet-facing seam。

## 现有项目比较

| 项目 | 维护状态（截至调查日） | Node 模型 | 动态 provisioning | logs / exec | 网络 | 存储 | 对 AG 的价值 |
|---|---|---|---|---|---|---|---|
| Virtual Kubelet | 活跃；v1.14.0，2026-09-07 | library 默认单静态 Node | 交给 provider | 标准 routes/helper | BYO | BYO/部分 projection glue | 直接依赖的 kubelet 兼容层 |
| InterLink | 活跃但 early development；stable 0.6.2，2026-07-10 | 配置式稳定 Virtual Node | plugin 决定 | logs 有；plugin ABI 无 exec/attach/port-forward | mesh 仍在演进 | core 以 Secret/ConfigMap/emptyDir 为主 | 可选兼容 driver，不作为 AG 核心 |
| Liqo | 活跃；v1.2.0，2026-07-03 | 每个 remote ResourceSlice/cluster 一个或多个 pool Node | 远端 K8s 自己调度 | status/logs/exec/attach/port-forward 转发 | 完整跨集群 fabric | virtual StorageClass + data gravity | 学 reflection、网络/存储诚实语义 |
| Admiralty | 未归档但较静止；最后 release/commit v0.17.0，2024-11-12 | 每个 target cluster 一个 pool Node | 远端候选 Pod | status/logs/exec 转发 | 依赖外部/商业网络方案 | ConfigMap/Secret 等依赖跟随；无通用 storage fabric | 学 candidate scheduling，不采用复杂双 scheduler |
| Elotl Kip | dormant；最后 commit 2021-09-09 | 一个稳定 pool Node；每 Pod 一台 VM cell | 是，绑定后启动/销毁 VM | 支持 logs/exec/stats | VPC IP + cell agent | 无 PV；只支持有限 volume；root disk | 最接近 AG 的历史原型，只学架构 |
| Azure ACI provider | 活跃；v1.6.4，2026-02-18 | 一个稳定 region/OS pool Node；每 Pod 一个 Container Group | 是，绑定后创建 ACI | logs/exec；attach/port-forward 不完整 | Azure CNI delegated subnet | inline volumes/Azure Files；AKS Virtual Nodes 不支持 PV/PVC | 最佳维护中 provider 参考实现 |
| AWS Fargate VK provider | 官方 repo 明示 inactive/seeking maintainers | 一个稳定 Fargate pool Node | 是 | 历史实现 | VPC | 很有限 | 不作为依赖；只作历史材料 |
| Karpenter | 活跃；v1.14.0，2026-07-11 | NodePool → immutable NodeClaim → 真实 Node/instance 1:1 | 是，在绑定前为 Pending Pods 建 Node | 真实 kubelet | 真实 Node CNI | 真实 Node CSI | 学 allocation journal/finalizer/状态，不是 Virtual Kubelet |
| KWOK | 活跃；v0.8.0，2026-06-23 | 一个 controller 模拟大量 Node/Pod | 否，只改 API status | 模拟 | 无真实 dataplane | 可模拟对象但无真实 I/O | AG 的规模/故障测试工具，不是 runtime |

这些项目均采用 Apache-2.0，InterLink 的部分独立 plugin 可能采用自己的许可证；接入时应逐个检查 driver repository。[VK license](https://github.com/virtual-kubelet/virtual-kubelet/blob/v1.14.0/LICENSE)、[InterLink license](https://github.com/interlink-hq/interLink/blob/main/LICENSE)、[Liqo license](https://github.com/liqotech/liqo/blob/master/LICENSE)、[Admiralty license](https://github.com/admiraltyio/admiralty/blob/master/LICENSE)、[Kip license](https://github.com/elotl/kip/blob/master/LICENSE)、[Karpenter license](https://github.com/kubernetes-sigs/karpenter/blob/main/LICENSE)、[KWOK license](https://github.com/kubernetes-sigs/kwok/blob/main/LICENSE)、[ACI license](https://github.com/virtual-kubelet/azure-aci/blob/v1.6.4/LICENSE)

### InterLink

[InterLink](https://github.com/interlink-hq/interLink) 的目标与 AGPI 很接近：Virtual Node 把 Pod 请求发给 InterLink API server，再由 provider-specific sidecar plugin 执行。项目活跃，stable `0.6.2` 于 2026-07-10 发布，之后还有 pre-release；但官方仍明确标记为 early development、可能 breaking，并把 long-running services 列为 out of scope。[Releases](https://github.com/interlink-hq/interLink/releases)、[Introduction](https://interlink-project.dev/docs/intro)、[Scope](https://github.com/interlink-hq/interLink#use-cases)

其 plugin API 很小：`POST /create`、`POST /delete`、`GET /status`、`GET /getLogs`，create request 包含 Pod、ConfigMaps、Secrets 和 projected volume maps。[Plugin guide](https://interlink-project.dev/docs/guides/develop-a-plugin)、[OpenAPI reference](https://interlink-project.dev/docs/guides/api-reference) 这对 SLURM/HTCondor batch 很合适，但不足以作为 AG 的完整 runtime ABI：没有 capability negotiation、Validate、Exec、Attach、PortForward、operation identity/adoption 和通用 data lifecycle。

网络方面，近期 InterLink Kubernetes plugin 提供 WireGuard-over-WebSocket mesh；volume 支持中的 PVC 仍标记 experimental，而且 core InterLink server 尚未完整支持。较稳定的公共交集仍是 Secret、ConfigMap 和 emptyDir。[Kubernetes plugin](https://github.com/interlink-hq/interlink-kubernetes-plugin)、[current limitations](https://interlink-hq.github.io/interLink/docs/Limitations/)

建议：

- 不把 InterLink server 放进 AG 必经路径；
- 实现一个 `ag-driver-interlink`，把 AGPI 的 batch-compatible 子集映射到 InterLink API；
- 该 driver capability 显式声明无 Exec/Attach/PortForward 或由独立 `ag-agent` 补足；
- 由此复用 InterLink 的 HPC plugins，同时不让其四个 endpoint 限制整个 AG 设计。

### Liqo

[Liqo](https://github.com/liqotech/liqo) 是当前最完整的 Virtual Node 系统之一。项目活跃，`v1.2.0` 于 2026-07-03 发布。它将 remote Kubernetes cluster 的资源切成一个或多个 `ResourceSlice`，每个 slice 可以产生一个 Virtual Node；consumer 的 vanilla scheduler 看到有限 capacity，provider 端再以 quota/admission 二次执法。[v1.2.0 release](https://github.com/liqotech/liqo/releases/tag/v1.2.0)、[offloading internals](https://docs.liqo.io/en/stable/advanced/peering/offloading-in-depth.html)、[resource reservation](https://docs.liqo.io/en/latest/usage/resource-reservation.html)

Pod 绑定到 Virtual Node 后，Liqo 创建 remote `ShadowPod`，再由 provider cluster 创建实际 Pod；ShadowPod 在 consumer 控制面短暂失联时仍维持 desired state。remote status 被映射回本地 Pod，kubelet routes 将 logs、exec、attach 和 port-forward 转发到 remote API server。[Offloading](https://docs.liqo.io/en/v1.0.0-rc.1/features/offloading.html)、[streaming route source](https://github.com/liqotech/liqo/blob/master/cmd/virtual-kubelet/root/http.go)、[remote exec/attach/port-forward source](https://github.com/liqotech/liqo/blob/master/pkg/virtualKubelet/reflection/workload/podns.go)

Liqo 的网络和存储比大多数 VK provider 完整：Geneve + WireGuard fabric 延伸 Pod/Service connectivity；virtual StorageClass 在实际 placement 后在目标集群创建真实 PVC，并用 PV affinity 贯彻 data gravity。跨集群迁移通过 Restic backup/restore，明确不支持 live migration。[Network Fabric](https://docs.liqo.io/en/stable/features/network-fabric.html)、[Storage Fabric](https://docs.liqo.io/en/stable/features/storage-fabric.html)、[Stateful Applications](https://docs.liqo.io/en/latest/usage/stateful-applications.html)

AG 应学习：

- Virtual Node 的 capacity 必须有供给侧二次执法；
- Pod 远端替身和 operation journal 能隔离控制面断连；
- 网络和存储若没有真正的数据面，就不能只同步 status 假装兼容；
- volume topology/data gravity 必须参与 placement。

AG 不应直接采用 Liqo，因为目标 provider 多数不是 Kubernetes cluster，也无法创建 ShadowPod/PVC/Service 等远端 Kubernetes 对象。Liqo 更适合作为未来 `ag-driver-kubernetes` 的实现参考。

### Admiralty

[Admiralty](https://github.com/admiraltyio/admiralty) 也把每个 target cluster 投影成 Virtual Node，但它没有相信汇总 capacity 能回答单个 Pod 是否可调度。source 侧 proxy scheduler 为每个 target 建 candidate Pod；target 侧 candidate scheduler 用真实集群状态完成 Filter/Reserve，source 再从可行 candidate 中选择一个 delegate。proxy Pod 的 status 映射 delegate Pod，ConfigMap/Secret、Service/Ingress 等依赖随 Pod 复制，logs/exec 被转发。[Scheduling architecture](https://github.com/admiraltyio/admiralty/blob/master/docs/concepts/scheduling.md)、[Introduction](https://github.com/admiraltyio/admiralty/blob/master/docs/introduction.md)

这是很有价值的反例：跨 cluster 时，向目标真正 dry-run/Reserve 比聚合 Node 信息准确。但 AG 的 marketplace provider 没有 Kubernetes scheduler 可以接收 candidate Pod；如果为每个 offer 做远程试创建，就会产生付费或难以回滚的副作用。AG 应保留“先验证单实例可行性”的思想，把它实现为 driver inventory snapshot + `Validate/Plan`，不要复制 Admiralty 的双 scheduler 协议。

项目没有归档，但最后 release/commit 是 `v0.17.0`（2024-11-12）；应视为可读的设计先例，而不是优先依赖。[v0.17.0 release](https://github.com/admiraltyio/admiralty/releases/tag/v0.17.0)、[last commit](https://github.com/admiraltyio/admiralty/commit/0dd77710a4b7daecf4e3dd4e01357ffc632f8565)

### Elotl Kip

[Kip](https://github.com/elotl/kip) 与 AG 的运行模型最接近。它创建一个 `kip-provider-0` Virtual Node；Pod 被默认 scheduler 绑定后，Kip 选择能满足 requests/limits 的最便宜 instance type，启动一台 cloud VM “cell”，在 cell 上用轻量 Itzo agent 运行 Pod，Pod 完成后终止实例。它还支持 warm standby cells。[README](https://github.com/elotl/kip/blob/master/README.md)、[cells](https://github.com/elotl/kip/blob/master/docs/cells.md)、[provider config](https://github.com/elotl/kip/blob/master/docs/provider-config.md)

Kip 曾支持 AWS/GCE、GPU、logs、exec、stats、probes、ServiceAccount token、VPC Pod IP、service proxy 和 NetworkPolicy。它不支持 PersistentVolume/Stateful workload，只物化 emptyDir、ConfigMap、Secret、hostPath 和 projected config；volumeMount 的 readOnly/subPath 等也不完整。root volume size 只是随 cell 生命周期的本地盘，不是 PVC。[feature/limitations](https://github.com/elotl/kip/blob/master/README.md#current-status)、[networking](https://github.com/elotl/kip/blob/master/docs/networking.md)、[volume-size annotation](https://github.com/elotl/kip/blob/master/docs/annotations.md)

Kip 的内部状态持久化在内嵌 etcd，并建议为 controller 挂 PVC。这再次验证 AG 需要独立于 Pod status 的 durable operation journal。[state](https://github.com/elotl/kip/blob/master/docs/state.md)

但 Kip 最后一次 commit 是 2021-09-09，依赖 Kubernetes 1.18 时代代码；只能做 architecture archaeology，不能 fork 为 AG。[commit history](https://github.com/elotl/kip/commits/master/)

### Azure ACI Virtual Kubelet provider

[Azure ACI provider](https://github.com/virtual-kubelet/azure-aci) 是当前最值得阅读的维护中 VK provider，`v1.6.4` 于 2026-02-18 发布。它投影一个稳定的 Linux/Windows、region-specific Virtual Node，每个 Kubernetes Pod 映射为一个 ACI Container Group；Pod UID/name/namespace/node 被写入 provider tags，方便关联和清理。[release](https://github.com/virtual-kubelet/azure-aci/releases/tag/v1.6.4)、[Pod translation source](https://github.com/virtual-kubelet/azure-aci/blob/v1.6.4/pkg/provider/aci.go#L316-L419)

它使用 provider API 创建/删除 Container Group，轮询状态并通过 `PodNotifier` 推送；logs 和 exec 映射 ACI API/WebSocket。[lifecycle/status](https://github.com/virtual-kubelet/azure-aci/blob/v1.6.4/pkg/provider/aci.go#L464-L477)、[status notifier](https://github.com/virtual-kubelet/azure-aci/blob/v1.6.4/pkg/provider/aci.go#L797-L845)、[logs/exec](https://github.com/virtual-kubelet/azure-aci/blob/v1.6.4/pkg/provider/aci.go#L548-L689)

需要明确避开的实现：`AttachToContainer` 仍为 placeholder；`PortForward` 记录 unsupported 却返回 nil，会向调用者制造 false success。AG 对任何 unsupported streaming capability 都必须返回明确错误，并在调度前通过 capability validation 尽量阻止不成立的语义。[attach source](https://github.com/virtual-kubelet/azure-aci/blob/v1.6.4/pkg/provider/aci.go#L692-L695)、[port-forward source](https://github.com/virtual-kubelet/azure-aci/blob/v1.6.4/pkg/provider/aci.go#L896-L899)

AKS managed Virtual Nodes 还说明完整网络/存储兼容需要 provider 基础设施：它要求 Azure CNI delegated subnet 才有 Pod-to-cluster 私网连接，并明确不支持 PV/PVC、NetworkPolicy 等场景；repo 支持 Azure Files 是 provider-specific inline translation，不等于通用 CSI。[AKS Virtual Nodes](https://learn.microsoft.com/en-us/azure/aks/virtual-nodes)、[volume translation](https://github.com/virtual-kubelet/azure-aci/blob/v1.6.4/pkg/provider/aci_volumes.go)

### Karpenter / NodeClaim

[Karpenter](https://github.com/kubernetes-sigs/karpenter) 不是 Virtual Kubelet，也不运行 Pod；它的价值是异步、付费 Node 生命周期建模。项目活跃，`v1.14.0` 于 2026-07-11 发布。[release](https://github.com/kubernetes-sigs/karpenter/releases/tag/v1.14.0)

Karpenter 观察 pending/unschedulable Pods，把 Pod constraints 与 NodePool/NodeClass 交叉计算，创建 immutable `NodeClaim`。NodeClaim 是 capacity request，并与 cloud instance 和最终 Kubernetes Node 1:1；状态分 launch、registration、initialization。创建失败时删除 NodeClaim并临时标记对应 instance type 不可用。[NodeClaims](https://karpenter.sh/docs/concepts/nodeclaims/)、[Scheduling](https://karpenter.sh/docs/concepts/scheduling/)

删除路径通过 Node/NodeClaim finalizer 执行 taint、drain、等待 VolumeAttachment 删除、终止实例，再移除 finalizer。AG 应采用同类显式状态和清理屏障，但对象改为 Pod UID 1:1 的 `AGAllocation`，因为 pool Virtual Node 不等于每个 provider instance。[Disruption and termination](https://karpenter.sh/docs/concepts/disruption/)

可借鉴：

- immutable intent + status conditions；
- provider ID 与 Kubernetes object 分开保存；
- creation、registration/runtime ready 分阶段；
- provider capacity error 有短期负缓存；
- finalizer 确保付费资源确实释放；
- controller 重启后依靠持久对象恢复，而不是内存 callback。

不应复制：Karpenter 最终等真实 kubelet注册真实 Node；AG 的 pool node 已存在，provider 资源不一定能运行 kubelet，也不能直接复用 NodeClaim controller。

### KWOK

[KWOK](https://github.com/kubernetes-sigs/kwok) 是重要的非运行时反例。它用一个 controller 管理大量任意创建的 fake Node，续租 Node Lease，并根据可配置 Stage 直接改 Pod status；它不运行 container，也没有真实网络、磁盘或日志流。[Architecture](https://github.com/kubernetes-sigs/kwok/blob/main/site/content/en/docs/design/architecture.md)、[manage Nodes and Pods](https://github.com/kubernetes-sigs/kwok/blob/main/site/content/en/docs/user/kwok-manage-nodes-and-pods.md)

项目活跃，`v0.8.0` 于 2026-06-23 发布。[release](https://github.com/kubernetes-sigs/kwok/releases/tag/v0.8.0) AG 不应使用 KWOK 作为 runtime 或 Node agent，因为把 status 改成 Running 并不产生 container；但应该把它用于：

- 数千 pool/Node 的 scheduler 与 controller scalability test；
- provider provisioning delay、spot interruption、NotReady 和删除失败的状态机模拟；
- 测试 admission/scheduler plugin，而不产生 GPU 费用。

### AWS Fargate 与旧 provider

旧的 open-source [AWS Fargate Virtual Kubelet provider](https://github.com/virtual-kubelet/aws-fargate) 在 README 中明确标注 inactive、seeking maintainers、not for production，不应依赖。旧 Alibaba ECI adapter 同样是 static pool node + selector/toleration 的历史样例，源码停留在 2020 年附近。[Fargate status](https://github.com/virtual-kubelet/aws-fargate#status)、[Alibaba ECI provider](https://github.com/virtual-kubelet/alibabacloud-eci)

当前 managed EKS Fargate 虽不是可复用开源实现，却验证了另一个模型：admission 根据 Fargate Profile 选择原生 Pod，专用 scheduler 调度，每个 Pod 获得独立 compute boundary。它不支持 DaemonSet、privileged、hostNetwork/hostPort、GPU、EBS 等完整 Node 语义；ephemeral disk 和 EFS 也有明确限制。[EKS Fargate](https://docs.aws.amazon.com/eks/latest/userguide/fargate.html)、[Pod configuration/storage](https://docs.aws.amazon.com/eks/latest/userguide/fargate-pod-configuration.html)

AG 可借其“native Pod + admission + custom scheduler + per-Pod isolation”思路，但实现仍应基于开放的 Virtual Kubelet/AGPI，而不是假定 Fargate 的闭源控制面可扩展。

## 网络、Streaming 和 Node endpoint

Virtual Node 的 `Node.status.addresses` 与 `daemonEndpoints.kubeletEndpoint` 必须指向 **Kubernetes control plane 能访问的 `ag-node`/gateway endpoint**，不能填 provider instance 的任意公网 IP。`kubectl logs/exec` 是 API server 调用 kubelet HTTPS endpoint，和 workload 自身网络是两条不同的数据面。

所有 AG Virtual Node 应设置 `node.kubernetes.io/exclude-from-external-load-balancers=true`，避免 cloud controller 把它们加入基础设施 LoadBalancer backend；ACI provider 也采用该标记。[ACI node labels](https://github.com/virtual-kubelet/azure-aci/blob/v1.6.4/pkg/provider/vk_node.go#L18-L36)

v1 可以分层声明网络能力：

- `ControlTunnel`：`ag-agent` 主动建立 mTLS/WebSocket/QUIC 通道，提供 status、logs、exec、attach、port-forward；所有实例型 provider 首先实现这个。
- `PublishedEndpoint`：driver 暴露 provider HTTP/TCP endpoint，但不宣称是 PodIP。
- `RoutablePodIP`：只有真正部署 overlay/VPN、DNS/Service routing 后才填写 PodIP 并允许需要 Pod 网络的 workload。

没有 `RoutablePodIP` 时，Service selector、NetworkPolicy、hostPort、Pod-to-Pod、ClusterFirst DNS 都不能被假状态模拟。Liqo 的完整 fabric 和 ACI 的 delegated subnet 恰好说明了成本所在。

Streaming 路径建议为：

```text
kubectl
  → kube-apiserver Pod subresource
  → ag-node shared kubelet HTTPS routes
  → AGAllocation lookup by namespace/name/UID
  → provider native API OR ag-gateway
  → outbound mTLS tunnel
  → ag-agent on remote resource
```

## Volume、Secret 与 PodSpec validation

Virtual Kubelet 只保留 PodSpec 的结构，不会自动保留每个字段的运行语义。

v1 建议：

| PodSpec 输入 | 行为 |
|---|---|
| `emptyDir` | 映射 provider/local scratch；随 allocation 删除 |
| `configMap`、`secret`、`projected`、`downwardAPI` | `ag-node` resolve 后加密发送给 driver/agent；driver 不访问 API server |
| `imagePullSecrets` | 解析成最小生命周期 registry credential；避免把 Kubernetes Secret 永久复制进 provider metadata |
| `image` volume / OCI artifact | driver 支持时 stage/mount，否则 capability error |
| PVC | 只有 pool/StorageClass 能保持 access mode、persistence、topology、fencing 时允许 |
| object-store stage/collect | 建模成单独 AG data binding，不伪装成 POSIX PVC |
| `hostPath`、device、mount propagation、未知 CSI | 拒绝调度 |
| provider 无法落实的 `securityContext` | admission warning + Event，继续运行并记录实际采用的 sandbox profile |

Volume 拒绝不是“字段洁癖”：一个挂载成功但内容不对、写入不持久或访问了错误路径的 Pod 可能产生不可恢复的数据损坏。SecurityContext 按当前产品选择先 warning，但 status 必须记录哪些约束未落实，便于审计。

## dynamic per-allocation Node 的后续路线

v1 不需要动态 Node，但未来若必须让 Node topology 严格对应真实租约，可以增加 `DedicatedLease` mode：

```text
Pod + schedulingGate
  → allocation controller 采购真实资源
  → 创建 ag-<allocation-uid> Virtual Node（pods=1）
  → 收紧 Pod placement 并移除 gate
  → default scheduler bind
  → shared ag-node runtime adopts allocation
```

这时 Node 更“诚实”，但成本也明确：

- stock VK 不管理动态 N Node；
- AG 要自己实现 shared informer、Node/Lease controller、共同 kubelet endpoint 和 serving certificate；
- Node churn 与 API server load 增加；
- Pod 还没绑定时就可能已经产生费用，取消和 orphan recovery 更复杂；
- marketplace offer 仍可能在采购中消失。

可将 Karpenter `NodeClaim` 的 immutable/finalizer/status 模式移植到 `AGAllocation`，但继续复用 VK 的 `node/api` routes。实现顺序应是：稳定 pool Node跑通完整 Pod lifecycle和数据安全，再评估是否真的需要 per-allocation Node；不要把它作为 v1 的前置条件。

## 建议的交付顺序

### Phase 0：VK conformance skeleton

- 新建独立 AG repo。
- 基于 Virtual Kubelet v1.14.x 实现 `ag-node` fake provider。
- 一个 `AGResourcePool` 对应一个 `ag-node` Deployment 和 Node。
- 跑通 Pod create/delete/status、logs/exec/attach/port-forward 的 contract tests。
- 对 unsupported 方法必须返回明确错误，禁止 ACI 式 false success。

### Phase 1：单 provider、pool node

- 实现 `AGProviderClass`、`AGResourcePool`、`AGAllocation`。
- 选择一个实例型 provider，实现 Pod UID tag/adopt、异步创建、poll/watch、终止和 orphan sweep。
- 提供 generic `ag-agent` control tunnel。
- 先支持 image、command/args/env、GPU、emptyDir、Secret/ConfigMap；PVC 拒绝。
- Node allocatable 使用保守 quota/并发预算，并按 inventory freshness 更新 Node Condition。

### Phase 2：scheduler 与多 provider

- 增加 inventory controller 和 immutable snapshot generation。
- 构建独立 kube-scheduler profile，Filter/Score 只读 snapshot。
- 加 RunPod、Vast、Modal 等 AGPI driver；所有 capability 差异在 `ValidatePod` 诊断中显式呈现。
- 增加 `ag-driver-interlink`，只宣称它能兑现的 batch/log 能力。

### Phase 3：数据与网络

- 实现 object-store stage/collect 与删除前 commit barrier。
- 对少数 provider 实现真实 provider-native volume adapter 和 topology。
- 先提供 port-forward/published endpoint；需要 Service 语义时再实现真实 overlay/routable PodIP。

### Phase 4：按证据决定 dynamic nodes

- 用 KWOK 和真实集群压测 Node churn、Lease、scheduler 和 kubelet route。
- 只有当 pool node 的 topology、capacity 或 isolation 语义确实阻塞用户时，才实现 shared `AGNodeManager` 与 `DedicatedLease`。
- pool node 与 dedicated node 共用 AGPI/AGAllocation，不让 driver 感知 Kubernetes Node 管理方式。

## 最终判断

AG 不需要发明“如何伪装成 kubelet”：Virtual Kubelet 已经把 Node/Pod/streaming 的 Kubernetes-facing 细节切成了可复用 library。AG 真正要发明的是它后面的深模块：异构 GPU 市场的 capability-aware placement、幂等付费 allocation、通用远端 agent、诚实的数据/网络语义。

因此 v1 的明确选择是：

```text
native Pod
  + one stable Virtual Node per AGResourcePool
  + Virtual Kubelet library
  + cache-only scheduler plugin
  + durable AGAllocation
  + out-of-process AGPI drivers
```

InterLink、Liqo、Admiralty、Kip、ACI、Karpenter 分别提供了 plugin boundary、跨集群 reflection、准确候选调度、per-Pod cloud VM、维护中 serverless provider 和异步 NodeClaim 生命周期的参考，但没有一个项目同时覆盖 AnyGPU 的 provider 市场、Pod compatibility 和数据安全边界。
