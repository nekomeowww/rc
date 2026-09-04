# macOS 作为 Kubernetes 节点：现有项目调查

调查日期：2026-08-29

## 结论

有现成项目，而且现在最值得研究的不是上游 kubelet 的 Darwin 移植，而是两个不同层次的自定义 node agent：

1. **首选参考：[`agoda-com/macOS-vz-kubelet`](https://github.com/agoda-com/macOS-vz-kubelet)**。它是目前最完整、可独立复用的开源实现：用 Virtual Kubelet 注册 Darwin 节点，每个 Pod 的第一个容器对应一台由 Apple Virtualization.framework 启动的 macOS VM；支持 OCI 分发、资源请求、状态、SSH exec/attach、部分 volume，以及可选 Docker sidecar。项目已有 `v1.4.1` release，并在 2026-07 仍有功能提交。[README / 功能矩阵](https://github.com/agoda-com/macOS-vz-kubelet/blob/b2616419f98e0b814891cd591489525fb3cee550/README.md)、[v1.4.1 release](https://github.com/agoda-com/macOS-vz-kubelet/releases/tag/v1.4.1)、[2026-07-06 提交](https://github.com/agoda-com/macOS-vz-kubelet/commit/b2616419f98e0b814891cd591489525fb3cee550)
2. **最强的生产案例：[`tuist/tuist/infra/tart-kubelet`](https://github.com/tuist/tuist/tree/1cdc7ccb35a80dce06ddc85686d9919c971576c9/infra/tart-kubelet)**。它不是通用库，也不是 Virtual Kubelet provider，而是 Tuist 自己的 controller-runtime node/pod agent：在 Mac mini 上注册 `kubernetes.io/os=darwin` 的真实 Node，把单容器 Pod 1:1 翻译为 Tart macOS VM，服务于 Xcode/GitHub Actions runner。源码显示它已有 finalizer、Node heartbeat、DiskPressure、ServiceAccount token、ConfigMap/Secret 环境解析、VM 垃圾回收、缓存盘、指标转发、VNC 等生产化机制；2026-05 至 2026-08 有密集的故障修复与容量演进。[入口说明](https://github.com/tuist/tuist/blob/1cdc7ccb35a80dce06ddc85686d9919c971576c9/infra/tart-kubelet/cmd/tart-kubelet/main.go)、[Node 注册实现](https://github.com/tuist/tuist/blob/1cdc7ccb35a80dce06ddc85686d9919c971576c9/infra/tart-kubelet/internal/nodeagent/node.go)、[Pod→Tart VM reconciler](https://github.com/tuist/tuist/blob/1cdc7ccb35a80dce06ddc85686d9919c971576c9/infra/tart-kubelet/internal/podagent/reconciler.go)、[近期双 VM 容量提交](https://github.com/tuist/tuist/commit/d9669fc518517183c01ec0d831cc9ff483bc3f2b)

如果 `rc` 的目标是 **Kubernetes 调度 Xcode/macOS CI**，应优先以 Agoda 的协议覆盖面和 Tuist 的生产运维经验为双重参考。若目标是 **直接在 macOS host 上执行进程**，RedpointGames 的实现是更小的原型。没有发现一个活跃、通用、接近上游 node conformance 的“原生 Darwin kubelet + Darwin CRI”。

## 分类与成熟度

| 项目 | 实际执行单元 | 节点实现 | 活跃度判断 | 适用性 |
|---|---|---|---|---|
| [`agoda-com/macOS-vz-kubelet`](https://github.com/agoda-com/macOS-vz-kubelet) | 每 Pod 一台 macOS VM；可带 Docker sidecar | Virtual Kubelet provider | **活跃、有 releases** | 最接近通用现成方案 |
| [`tuist/tuist` 的 `tart-kubelet`](https://github.com/tuist/tuist/tree/1cdc7ccb35a80dce06ddc85686d9919c971576c9/infra/tart-kubelet) | 每 Pod 一台 Tart macOS VM | 自定义 controller-runtime agent | **活跃、明显在生产演进** | Xcode/GitHub Actions CI 最强案例 |
| [`RedpointGames/macos-virtual-kubelet`](https://github.com/RedpointGames/macos-virtual-kubelet) | macOS host 上的 `launchd` 进程 | Virtual Kubelet provider | **原型；2025-05 后无代码提交、无 release** | host-process 语义参考 |
| [`Raikerian/macos-virtual-kubelet`](https://github.com/Raikerian/macos-virtual-kubelet) | 每 Pod 一台 Virtualization.framework macOS VM | Virtual Kubelet provider | **未完成；2023-12 后无代码提交、无 release** | 早期骨架，不宜直接采用 |
| [`makllama/makllama`](https://github.com/makllama/makllama) + forks | macOS host 上的专用 LLM runtime | Virtual Kubelet CRI provider + forked containerd shim | **实验；2024-05 后无代码提交、无 release，关键源码缺失** | 原生 Darwin runtime 思路验证，不是通用容器方案 |
| [`saiyam1814/kiac`](https://github.com/saiyam1814/kiac) / [`apple/container`](https://github.com/apple/container) | Linux VM 内的标准 kubelet/containerd | 标准 Linux Node | **活跃** | 相邻方案；不是 Darwin Node、不能跑 Xcode |

## 重点项目

### 1. Agoda macOS-vz-kubelet：最完整的独立开源实现

它明确采用 Virtual Kubelet，而非移植上游 kubelet。Mac host 运行 provider；Pod 第一个 container 的 `image` 是自定义 OCI macOS VM artifact，CPU/内存 request 决定 VM 规格；镜像通过 ORAS 拉取并用 APFS `clonefile` 做写时复制。默认 NAT，也支持需要 Apple entitlement 的 bridged networking。[架构与镜像说明](https://github.com/agoda-com/macOS-vz-kubelet/blob/b2616419f98e0b814891cd591489525fb3cee550/README.md#how-it-works)、[VM 资源管理源码](https://github.com/agoda-com/macOS-vz-kubelet/blob/b2616419f98e0b814891cd591489525fb3cee550/pkg/resourcemanager/macos.go)

它已经覆盖了相当多 kubelet-facing 行为：Pod create/delete/status，macOS VM 与 Docker sidecar，SSH exec/attach，双方 metrics，host/emptyDir/projected volume，imagePullSecrets，以及 SSH readiness/postStart gate。实现也真实转发了 Virtual Kubelet 的 logs/exec/attach API，而不是只“注册一个 Node”。[provider 方法](https://github.com/agoda-com/macOS-vz-kubelet/blob/b2616419f98e0b814891cd591489525fb3cee550/pkg/provider/macosvz.go)、[SSH exec 实现](https://github.com/agoda-com/macOS-vz-kubelet/blob/b2616419f98e0b814891cd591489525fb3cee550/pkg/resourcemanager/macos_exec.go)

边界也写得很清楚：不支持 Pod update、init containers、security policy、resource limits、Pod-spec probes、PV、ConfigMap/Secret volume 和 port-forward；VM 内 exec/attach、readiness、stats 都依赖预配置 SSH。它因此是“可用的专用节点”，不是标准 kubelet 的等价替代。[官方功能矩阵](https://github.com/agoda-com/macOS-vz-kubelet/blob/b2616419f98e0b814891cd591489525fb3cee550/README.md#feature-overview)

### 2. Tuist tart-kubelet：最有说服力的 macOS CI 实战

Tuist 的实现直接 watch `spec.nodeName` 指向本机的 Pod，自行创建和维护 Node，不依赖 Virtual Kubelet 库。Node 固定发布 `kubernetes.io/os=darwin`、`kubernetes.io/arch=arm64`、`tuist.dev/runtime=tart`，并加 `tuist.dev/macos:NoSchedule` taint；状态包含 Ready、DiskPressure、容量与 runtime 信息。[Node 配置源码](https://github.com/tuist/tuist/blob/1cdc7ccb35a80dce06ddc85686d9919c971576c9/infra/tart-kubelet/internal/nodeagent/node.go#L259-L375)

Pod contract 是刻意收窄的：Pod 与 Tart VM 1:1，只处理单 regular-container、无 init/ephemeral container 的 Pod；container image 实际是 Tart VM image。reconciler 负责 clone/pull/run/delete、状态与退出码、finalizer、重启恢复和孤儿 VM 清理。[reconciler 顶部契约与过滤](https://github.com/tuist/tuist/blob/1cdc7ccb35a80dce06ddc85686d9919c971576c9/infra/tart-kubelet/internal/podagent/reconciler.go#L1-L202)、[创建 VM 路径](https://github.com/tuist/tuist/blob/1cdc7ccb35a80dce06ddc85686d9919c971576c9/infra/tart-kubelet/internal/podagent/reconciler.go#L392-L606)

项目历史比 star 数更有意义：路径自 2026-05 加入后，持续出现 VM disk leak、readiness、kubelet restart rebind、golden image cache、DiskPressure、runner cache volume、Xcode CAS、交互访问和双 guest 并发等提交。这是强烈的生产使用信号，而不只是 README demo。[首次加入该路径的提交](https://github.com/tuist/tuist/commit/a15a84f2c01132368ebc5780e5224e05ea92184e)、[重启恢复提交](https://github.com/tuist/tuist/commit/30eba28f43e7c605e0c265f98c8821b5b5cbb834)、[Xcode CAS 提交](https://github.com/tuist/tuist/commit/2ffa31c9676ff419aedce1ca9855f5860090100e)

缺点是它与 Tuist runner 协议、Tart、缓存和服务端 label/annotation 强耦合，没有独立 release，也没有抽成通用 provider。适合“读源码、提炼机制”，不适合直接当依赖。

### 3. RedpointGames：直接运行 host process 的最小实现

该项目将 Pod 转成用户 `~/Library/LaunchAgents` 下的 plist，再调用 `launchctl load/start`；退出状态来自 `launchctl list`，container logs 读取临时文件。README 明确警告进程没有虚拟化、安全隔离或 Pod 间隔离，应视为类似 Windows HostProcess container。[README](https://github.com/RedpointGames/macos-virtual-kubelet/blob/bf2782ba163ddb907859a07ba7aee891ae3b721e/README.md)、[provider 源码](https://github.com/RedpointGames/macos-virtual-kubelet/blob/bf2782ba163ddb907859a07ba7aee891ae3b721e/provider.go)

它能说明“用 Virtual Kubelet 将 Kubernetes Job 映射到 Mac host process”所需的最小面，但 exec、attach、stats、metrics、port-forward 都直接返回 `not implemented`，仓库没有 release，代码提交停在 2025-05-02。[未实现接口](https://github.com/RedpointGames/macos-virtual-kubelet/blob/bf2782ba163ddb907859a07ba7aee891ae3b721e/provider.go#L226-L256)、[最后提交](https://github.com/RedpointGames/macos-virtual-kubelet/commit/bf2782ba163ddb907859a07ba7aee891ae3b721e)

### 4. Raikerian：Virtualization.framework 原型，基本停滞

它也使用 Virtual Kubelet，并用 `Code-Hex/vz` 为每个 Pod 创建 macOS VM。但 ResourceManager 只读取第一个 container 的 CPU/memory、启动 VM 并把对象放进内存 map；没有体现容器 command/image 如何进入 guest，也没有持久化恢复，logs/exec/attach/stats/metrics/port-forward 全未实现。[ResourceManager](https://github.com/Raikerian/macos-virtual-kubelet/blob/09bfeb96cef3cae7cf4615a75076ef2ab1fd5986/internal/manager/resource.go)、[provider 未实现接口](https://github.com/Raikerian/macos-virtual-kubelet/blob/09bfeb96cef3cae7cf4615a75076ef2ab1fd5986/provider/macos.go)

最后代码提交为 2023-12-24，且无 release；它更像 Agoda 项目出现前的探索性骨架。[最后提交](https://github.com/Raikerian/macos-virtual-kubelet/commit/09bfeb96cef3cae7cf4615a75076ef2ab1fd5986)

### 5. MaKllama：最接近“Darwin CRI”的实验，但不是通用实现

MaKllama 的组合是 forked Virtual Kubelet CRI provider、forked containerd、名为 `runm` 的 LLM runtime 和名为 Bronze Willow 的 macOS CNI；README 展示 Mac 节点注册并调度 TinyLlama Deployment。[项目说明与 demo](https://github.com/makllama/makllama/blob/1938962ace2dd6de5c1a5e8e4f25e5cf592dd206/README.md)

源码检查显示，CRI fork 对 Darwin 的改动主要是 socket/volume 路径、`sw_vers` NodeInfo 和 ReplicaSet 状态拼装；containerd fork 移除部分 Linux build constraints/cgroup/mount 路径，将 shim 改为调用专用 runm。[CRI Darwin 提交](https://github.com/makllama/cri/commit/138e771138ce44df7ff8202e21ec758ed4c1b4b3)、[containerd shim 提交](https://github.com/makllama/containerd/commit/69c909dac406953de40bacce3f808f527578d2fb)

但它不构成可复用的开源 Darwin container stack：README 明说 `runm` 和 Bronze Willow “source code will be available soon”，主仓库实际分发预编译 binary；示例 image 是 Ollama model，command 还是规避 containerd 检查的 dummy，而非普通 macOS OCI userspace。[组件公开状态](https://github.com/makllama/makllama/blob/1938962ace2dd6de5c1a5e8e4f25e5cf592dd206/README.md#main-components)、[TinyLlama manifest](https://github.com/makllama/makllama/blob/1938962ace2dd6de5c1a5e8e4f25e5cf592dd206/k8s/tinyllama.yml)

主仓库及两个 fork 都在 2024-05 后没有代码提交，也没有 release。因此应判定为 LLM/Apple Silicon 专用 proof of concept，不能作为今天实现 Xcode/macOS CI 节点的基础。

## 容易误判的相邻项目

[`apple/container`](https://github.com/apple/container) 是在 Mac 上以轻量 VM 运行 **Linux containers** 的工具，不会把 macOS/Darwin 注册成 Node。[项目定义](https://github.com/apple/container)

[`saiyam1814/kiac`](https://github.com/saiyam1814/kiac) 利用 apple/container 为每个 Kubernetes Node 启动独立的 Linux VM，VM 内运行标准 kubelet、cgroups 和 containerd；示例 Node 的 kernel 是 Linux 6.12，runtime 是 containerd。它是很好的“Mac 承载 Linux Kubernetes”方案，但不能执行 Xcode 或原生 macOS workload。[架构说明](https://github.com/saiyam1814/kiac/blob/88138a3ae5121926193ca5840bad3d089e0830d8/README.md#why-this-matters)、[运行结果](https://github.com/saiyam1814/kiac/blob/88138a3ae5121926193ca5840bad3d089e0830d8/README.md#see-the-isolation-pay-off)

传统 Lima/Colima/Docker Desktop 内运行 kubelet也属于同一类：宿主机是 Mac，但 Kubernetes Node 仍是 Linux VM。

## 对 rc 的建议

若要实现一个面向 Xcode/macOS CI 的节点，最稳妥的边界是：

- 自定义 node agent 或 Virtual Kubelet provider，而非 fork 上游 kubelet；
- Pod 1:1 macOS VM，第一版只支持单容器 Job；
- 专用 `darwin` label、runtime label 与 `NoSchedule` taint；
- image 字段明确解释为 Tart/Virtualization.framework VM artifact；
- 优先实现 create/delete/status、finalizer、重启恢复、logs/退出码、Secret/ConfigMap env、ServiceAccount token、磁盘压力和孤儿 VM GC；
- 不承诺 CNI/Service/PV/sidecar/通用 OCI conformance。

技术选型上，**Agoda 适合复用 Virtual Kubelet API 层与功能矩阵，Tuist 适合复用 controller-runtime reconciliation、Tart 生命周期和生产故障处理思路**。如果 `rc` 只需运行可信的本机构建进程而不需 VM 隔离，再考虑 RedpointGames 的 launchd provider 模式。

## 调查方法

本调查使用 GitHub CLI 的 repository/code search 搜索 `macos kubelet`、`darwin kubelet`、`macos virtual kubelet`、`macOS-vz-kubelet`、`kubernetes.io/os=darwin`、`tart-kubelet`、`macos virtualization kubernetes`、`macos kubernetes node xcode`、`kiac` 等词；随后 clone 候选仓库，检查 provider、node registration、VM/runtime、Pod lifecycle、未实现接口、commit history、tags 与 releases。结论仅引用项目自身 README、源码、commit 和 release 等一手来源。
