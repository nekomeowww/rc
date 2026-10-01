# AnyGPU provider API 与远端存储调查

调查日期：2026-09-09

## 结论

这五个平台不能用一个“租机器”的接口等量齐观。它们至少分成三种运行模型：

1. **GPU Pod / VM 租赁**：RunPod、Vast.ai、AutoDL。AG Driver 主要负责选择库存、创建实例、轮询状态、连接容器或 VM，以及销毁计费资源。
2. **Serverless container runtime**：Modal。AG Driver 面对的是 Sandbox、Function call、Volume 等平台对象，不是可永久持有的一台机器。
3. **容器任务与服务编排**：共绩算力（“算了么”的需求侧产品）。它已经有弹性部署、Job 队列、节点、日志、事件和多类存储 API，资源模型反而最接近 `AGJob`/`AGService`。

因此，AGPI 应保持 **Pod 生命周期协议**，不要把任一厂商的 instance、function 或 task DTO 暴露为公共模型。Provider Driver 把同一个不可变 `core/v1 Pod` blueprint 编译为自己的执行计划，并用 `providerID` 保存平台对象的真实身份。

存储也不应被简化为“上传一个目录”。建议把数据分成三类：

- `emptyDir`/root disk：临时工作区，Pod 删除后允许消失；
- provider-native volume：持久、低延迟，但绑定 provider/region/zone；
- object storage：跨 provider 的真实数据平面，用于代码包、dataset、model、checkpoint 和最终产物。

对 `rc` 最重要的结论是：**任何本地 Kubernetes PVC 都不能自动变成远端可挂载的卷**。若要在多个 GPU 平台之间迁移 Workspace，权威副本必须放在所有 Driver 都能访问的对象存储或网络文件系统中；provider-native volume 只能作为缓存或带显式亲和性的工作副本。

## 调查范围与判断标准

本报告只使用厂商自己的文档、API 文档和官方 SDK/CLI 源码。每个 provider 按以下问题检查：

- 身份认证以及 API、CLI、SDK 可用性；
- 搜索库存，创建、启动、停止、删除资源；
- 异步状态、事件、日志与失败恢复；
- 幂等键、名称、标签或其他可恢复标识；
- OCI image、command、args、env、secret 和 GPU 约束；
- exec、SSH、端口和公网 endpoint；
- 本地盘、持久卷、快照、对象存储和数据传输；
- 与 AGPI `ValidatePod`、`EnsurePod`、`Get/WatchPodStatus`、`DeletePod`、`Exec`、`Logs`、`PortForward` 的映射。

“文档没有公开某能力”只表示本次在一手资料中没有找到稳定契约，不等于平台内部绝对没有该能力。

## Provider 调查

### RunPod

#### API、认证与资源模型

新接入应使用已经 GA 的 REST API v2，base URL 为 `https://api.runpod.io/v2`，Bearer API key 可以设置权限范围，官方发布 OpenAPI document。REST v1 计划于 2026-11-15 退役，GraphQL 计划于 2027 年初退役；因此 AG Driver 不应以旧 GraphQL client 作为长期控制面。[REST v2 概览与 OpenAPI](https://docs.runpod.io/api-reference-v2/overview)、[发布与退役时间表](https://docs.runpod.io/release-notes)

核心资源是一个单容器 Pod。REST v2 提供 list/create/get/update/terminate，并把 start/stop/restart/terminate 统一为 `POST /v2/pods/{id}/action`。状态包括 `PROVISIONING`、`STARTING`、`RUNNING`、`EXITED`、`ERROR`、`TERMINATED`；响应还返回当前允许的 actions，Driver 应使用它判断合法转换，而不是只硬编码状态机。[v1→v2 迁移说明](https://docs.runpod.io/api-reference-v2/migrate-from-v1)、[创建 Pod](https://docs.runpod.io/api-reference-v2/pods/create-a-pod)

#### 创建、调度与幂等

创建请求支持 image、传给 image entrypoint 的单个 args 字符串、env、ports、私有 Registry Credential、GPU/CPU、cloud tier、data center preference、container/pod disk 和 network volume。GPU Catalog 可查询型号、VRAM、Secure/Community Cloud 的价格及各数据中心库存，并支持 GPU ID/count、CUDA、每卡最小 RAM、最小 vCPU 等约束。[创建 Pod](https://docs.runpod.io/api-reference-v2/pods/create-a-pod)、[GPU Catalog](https://docs.runpod.io/api-reference-v2/catalog/list-gpu-types)

当前 v2 create schema 没有最高价格和 Spot/interruptible 字段，而即将退役的 v1 仍有 `interruptible`。首版 Driver 应在 create 前用 Catalog 校验 `maxPrice`，创建后再检查响应里的实际 `cost`，并明确报告两次调用之间的价格/库存竞态；Spot policy 应报告 unsupported，不能为它长期绑定旧 API。[v2 create](https://docs.runpod.io/api-reference-v2/pods/create-a-pod)、[v1 create](https://docs.runpod.io/api-reference/pods/POST/pods)

v2 创建/列表 schema 没有公开 label/tag、name filter 或 idempotency key，也未承诺 name 唯一。`EnsurePod` 需要自己的 durable operation journal；遇到 create response 丢失时，只能全量 list 后按确定性 name/time/spec 做弱恢复，拿到 Pod ID 后立即持久化，并对重复对象做隔离/GC。name 不是 exactly-once 保证。[创建 Pod schema](https://docs.runpod.io/api-reference-v2/pods/create-a-pod)

#### PodSpec、日志与网络

RunPod 是单容器模型，并且 v2 只有 image entrypoint args，不能直接表达 Kubernetes `command` 覆盖和 `args` 的完整组合。Secret 不应直接永久展开到可被 Pod query 返回的 provider env；私有 image 应使用独立 Registry Credential。SSH 依靠 `PUBLIC_KEY` 和镜像内 sshd，自定义 image 需要主动满足这个约定。[创建参数](https://docs.runpod.io/api-reference-v2/pods/create-a-pod)、[SSH 约定](https://docs.runpod.io/pods/configuration/use-ssh)

日志接口 `GET /v2/pods/{id}/logs` 是 SSE，支持 container/system source、`tail`、`since` 和 `Last-Event-ID` 恢复游标，能直接实现 `Logs(follow=true)`。REST v2 没有 Pod exec/attach 或状态 watch；状态需要轮询，交互式 exec/TTY 和任意端口 tunnel 需要 SSH 或 AG guest agent。[日志流](https://docs.runpod.io/api-reference-v2/pods/stream-pod-logs)、[SSH](https://docs.runpod.io/pods/configuration/use-ssh)

平台可暴露 HTTP Proxy 和直接 TCP，但不支持 UDP；HTTP Proxy URL 是公网 endpoint，不等价于 Kubernetes `port-forward`。[端口说明](https://docs.runpod.io/pods/configuration/expose-ports)

#### 硬盘和数据

RunPod 有三种不同语义的盘：[Pod storage types](https://docs.runpod.io/pods/storage/types)

- container disk 是本地临时盘，stop/restart 会丢失；
- pod volume disk 是本地持久盘，stop/restart 保留，但删除 Pod 时随 Pod 删除；
- network volume 独立于 Pod，可共享和重新挂载，删除 Pod 后保留。

Network Volume 只支持 Secure Cloud，必须在创建 Pod 时绑定，之后不能 attach/detach；一个 Pod 当前最多使用一个 persistent 或 network mount，volume type/ID 创建后不可变。Volume 固定在 data center，会直接收窄可用 GPU 库存。[Network Volume](https://docs.runpod.io/storage/network-volumes)、[v2 create](https://docs.runpod.io/api-reference-v2/pods/create-a-pod)

挂载 Network Volume 的 Pod 不能由用户 stop，只能 terminate；Network Volume 本身继续保留。这意味着 AG 的“暂停计算但保留同一个 Pod”对这种组合不可用，必须把 suspend 实现为删除计算、保留 Volume，resume 时重建 Pod。[Network Volume](https://docs.runpod.io/storage/network-volumes)

部分数据中心允许通过独立 S3-compatible API 在没有计算实例时向 Network Volume 导入/导出数据，需要单独 S3 key。普通 Pod/data disk 还可通过 `runpodctl send/receive`、SCP/rsync 或 Cloud Sync 与 S3、GCS、Azure、Backblaze、Dropbox 传输；one-time-code 传输适合偶发小规模操作，AG 自动化应优先对象存储和可恢复的增量同步。官方也建议不要把 RunPod 当长期云存储，关键数据另行备份；公开 v2 资源中没有 snapshot primitive。[S3 API](https://docs.runpod.io/storage/s3-api)、[文件传输](https://docs.runpod.io/pods/storage/transfer-files)、[Cloud Sync](https://docs.runpod.io/pods/storage/cloud-sync)、[价格与存储提示](https://docs.runpod.io/pods/pricing)

#### CLI/SDK 与 AGPI 适配

`runpodctl pod` 支持 create/list/get/start/stop/restart/update/delete，适合人工诊断；官方 Python SDK 的 Pod 管理主要仍包装 GraphQL，另有官方 Go SDK。Driver 最稳妥的实现是直接调用 REST v2，或从 v2 OpenAPI 生成 typed client，而不是 shell-out CLI。[runpodctl pod](https://docs.runpod.io/runpodctl/reference/runpodctl-pod)、[Python SDK](https://github.com/runpod/runpod-python)、[Go SDK](https://github.com/runpod/go-sdk)

| AGPI 方法 | RunPod Driver |
|---|---|
| `ValidatePod` | 单容器、GPU/CUDA、cloud tier、data center、价格、仅 TCP/HTTP、至多一个 provider mount；v2 Spot 拒绝 |
| `EnsurePod` | Catalog/volume 查询后 create，立即保存 Pod ID；无强恢复键，只能 journal + full-list 弱恢复 + duplicate GC |
| `GetPodStatus` | GET Pod，映射六种状态，并记录 cost/data center/endpoints |
| `WatchPodStatus` | Driver 轮询；只有 logs 是原生 SSE |
| `DeletePod` | terminate Pod；network volume retention 独立处理；带 Network Volume 时不支持 stop |
| `Logs` | 强：SSE + cursor |
| `Exec` / `Attach` | SSH 或 AG guest agent |
| `PortForward` | SSH tunnel；HTTP proxy 另表为 endpoint capability |

### Vast.ai

#### API、认证与市场模型

Vast.ai 提供以 `https://console.vast.ai/api/v0` 为主的 REST API，官方 `vastai` CLI 和 Python SDK 也建立在该 API 上；Bearer API key 可以限定 scopes。Driver 至少需要 instance read/write，账户 Secret 等功能需要额外权限。[API 概览](https://docs.vast.ai/api-reference/introduction)、[认证](https://docs.vast.ai/api-reference/authentication)、[权限](https://docs.vast.ai/api-reference/permissions)

创建不是选择固定 SKU，而是两阶段市场交易：先 `POST /api/v0/bundles` 搜索 offer，再 `PUT /api/v0/asks/{offer_id}` 接受具体 offer 并得到 `new_contract` instance ID。多数 instance operation 仍是 `/api/v0`，新的分页 list 使用 `/api/v1/instances`；Driver 应把这种版本差异封在内部。[搜索 offer](https://docs.vast.ai/api-reference/search/search-offers)、[创建流程](https://docs.vast.ai/api-reference/creating-instances-with-api)、[实例列表](https://docs.vast.ai/api-reference/instances/show-instances)

#### 搜索、生命周期与幂等

offer 可按 GPU model/count/VRAM、CUDA、reliability、verified、geolocation、disk、bandwidth、direct ports 和 `dph_total` 价格上限筛选；支持 on-demand、bid 和 reserved。Bid/interruptible 通过搜索 `type=bid` 并在接受 offer 时出价。[搜索接口](https://docs.vast.ai/api-reference/search/search-offers)、[创建指南](https://docs.vast.ai/api-reference/creating-instances-with-api)

offer 会被其他买家抢走。`EnsurePod` 必须把 search→try offer→库存冲突→search next 作为正常 saga，不能把 offer ID 当稳定机型。Instance 可 start/stop/label，destroy 不可逆且会删除 instance local data。[管理实例](https://docs.vast.ai/api-reference/instances/manage-instance)、[销毁实例](https://docs.vast.ai/api-reference/instances/destroy-instance)

状态只能 query instance/list，没有公开 watch/event stream；`actual_status` 除 loading/running/stopped 外还有 frozen、exited、rebooting、unknown、offline 等状态，其中 frozen 仍收 GPU 费，unknown/offline 表示 host 心跳或连接异常。Driver 不能把所有非 running 状态都当作普通 Pending；exited/unknown/offline 应进入失败或重新调度判断。API 还有 endpoint-level rate limits，429 不保证带 `Retry-After`。create 无 idempotency key，但 instance 支持 label，v1 list 可按 label filter；label 并非唯一。Driver 应用确定性 `ag-<cluster>-<namespace>-<uid>` label 恢复，同时检测多匹配并 GC。[实例状态](https://docs.vast.ai/cli/reference/show-instance)、[速率限制](https://docs.vast.ai/api-reference/rate-limits-and-errors)、[实例列表与 label](https://docs.vast.ai/api-reference/instances/show-instances)

#### PodSpec、交互和网络

创建支持 image/template、label、disk、`runtype`、desired state、env、`onstart`、`args/args_str`、私有 image login 和 `volume_info`。不同 SSH/Jupyter runtype 会改变启动行为，无法自然复现所有 Kubernetes command/args 语义。[创建指南](https://docs.vast.ai/api-reference/creating-instances-with-api)

账户级加密 env 会注入账户下实例，不适合动态承载 namespace Pod Secret；只应用于静态共享凭据。Pod 级 Secret 应由 AG guest agent 通过一次性凭据接收，`image_login` 同样不能写入 CR status、event 或 log。[Docker env](https://docs.vast.ai/guides/instances/docker-environment)

原生 execute `PUT /api/v0/instances/command/{id}` 只接受最长 512 字符的非交互命令，异步返回 S3 result URL。Logs API 先要求实例上传日志，再返回 S3 URL，可筛选 container/daemon 和 tail，但不能 follow。SSH 可做 remote command、SCP/SFTP 和 tunnel；Docker 端口映射的外部端口可能随机。[Execute API](https://docs.vast.ai/api-reference/instances/execute)、[Logs API](https://docs.vast.ai/api-reference/instances/show-logs)、[SSH 指南](https://docs.vast.ai/guides/instances/connect/ssh)、[网络说明](https://docs.vast.ai/guides/instances/connect/networking)

所以非交互 `Exec` 可使用 execute API；TTY/attach/port-forward 需要 SSH；`Logs(follow=true)` 需要轮询日志快照，或由 agent/SSH `tail -F` 提供。

#### 硬盘和数据

Instance/container storage 在 stop 时保留且继续收费，destroy 时删除。独立 Volume 可以在 instance 删除后保留，但它绑定创建它的物理 host、固定大小、不可迁移；复用时必须只搜索该物理机的 offer。Volume 被 running 或 stopped instance 挂载时也不能直接删除，需先 destroy 相关 instance。它的诚实语义是 `LocalPersistentVolume + nodeAffinity`，不是通用云盘。[Storage Types](https://docs.vast.ai/guides/instances/storage/types)、[Volumes](https://docs.vast.ai/guides/instances/storage/volumes)

Volume 与 instance 之间可复制；本机、instance 与外部云之间可用 `vastai copy`/rsync、SCP/SFTP 或 Cloud Copy。Vast 没有类似 RunPod Network Volume 的公共对象存储，Cloud Sync 连接 S3、Google Drive、Backblaze、Dropbox 等外部服务；公开资源中也没有 snapshot primitive。因此 checkpoint 应持续写入外部 object storage，Vast Volume 只作为带 host affinity 的热缓存或工作盘。[数据移动](https://docs.vast.ai/guides/instances/storage/data-movement)、[Cloud Sync](https://docs.vast.ai/guides/instances/storage/cloud-sync)

#### CLI/SDK 与 AGPI 适配

官方 `vastai` CLI 和 Python `VastAI` SDK 覆盖 search/create/show/start/stop/destroy、volume、copy 和 logs。[官方 CLI/SDK 仓库](https://github.com/vast-ai/vast-cli)。Go Driver 可直接封装 REST 并对 v0/v1 做契约测试，或把官方 Python SDK 放入独立 sidecar；不建议 Controller 直接 shell-out CLI。

| AGPI 方法 | Vast.ai Driver |
|---|---|
| `ValidatePod` | 单容器、offer constraints、max price、reliability、ports、Volume host affinity；区分 on-demand/bid |
| `EnsurePod` | 搜索并尝试候选 offer，offer 消失时重搜；按确定性 label 恢复并检测重复 |
| `GetPodStatus` | 显式映射 loading/running/stopped/frozen/exited/rebooting/unknown/offline、价格、IP/SSH；bid 中断映射 `Interrupted` |
| `WatchPodStatus` | 退避轮询并处理 429 |
| `DeletePod` | destroy instance；随后才能按 retention policy 删除已解绑 Volume |
| `Logs` | S3 snapshot；follow 需要轮询或 agent/SSH |
| `Exec` / `Attach` | 短命令可用 execute；TTY 用 SSH/agent |
| `PortForward` | SSH tunnel；公网端口另表为 endpoint capability |

### Modal

#### API、认证与资源模型

自动化账号使用 `MODAL_TOKEN_ID` 和 `MODAL_TOKEN_SECRET`。Service User 需要在目标 Environment 中被授予 Viewer 或 Contributor，且只有 Team/Enterprise 可用；个人 token 也使用相同环境变量。[Service Users](https://modal.com/docs/guide/service-users)、[Go SDK 配置](https://modal.com/docs/sdk/go/latest)

Modal 提供官方 Python SDK、Beta 的 JavaScript/TypeScript 和 Go SDK，以及 `modal` CLI；没有面向用户承诺的通用 REST 控制面。因此 Driver 应直接使用 Go/Python SDK，不能反向依赖内部 RPC。[JavaScript/Go SDK](https://modal.com/docs/guide/sdk-javascript-go)、[CLI reference](https://modal.com/docs/cli/latest)

最适合映射 AGPod 的资源是关联到 App 的 Sandbox。Sandbox 有 entrypoint process、额外 exec processes、tunnels 以及挂载的 Volume/CloudBucketMount；Driver 可先 `App.lookup(..., create_if_missing=true)`。[Sandboxes](https://modal.com/docs/guide/sandboxes)

#### 生命周期与幂等

SDK 支持 `Sandbox.create`、`from_id`、`from_name`、`list`、`poll`/`wait` 和 `terminate`。生命周期为 Created→Scheduled→Started→Ready（可选）→Finished；默认最大寿命五分钟，可配置到 24 小时，超过 24 小时需 snapshot 后重建。[Sandbox lifecycle](https://modal.com/docs/guide/sandboxes)

同一 deployed App 内，named Sandbox 在运行期间唯一，重复创建会返回 `AlreadyExistsError`；结束后 name 可复用，`from_name` 只能找当前运行的对象。Sandbox 还支持任意 key/value tags 及按 tag list。因此 Driver 应使用由 AG UID 派生的 name，并写 UID/namespace/name/specDigest tags，但仍要持久化 Sandbox ID 以追踪终态对象。[Named Sandboxes and tags](https://modal.com/docs/guide/sandboxes)

公开 Sandbox API 没有 lifecycle event subscription，`WatchPodStatus` 需要 poll readiness/exit code。`terminate` 对已结束 Sandbox 是 no-op，天然适合幂等 delete；AGPI 不需要向上暴露 Sandbox 不具备的 start/stop/restart。[Sandbox API](https://modal.com/docs/sdk/py/latest/Sandbox)

#### PodSpec、交互和网络

Sandbox create 原生支持 image、command、workdir、env、Secret、CPU request/limit、memory request/limit、GPU、region/cloud、Volume/CloudBucketMount、readiness probe、PTY 和 encrypted/unencrypted/HTTP2 ports。Go SDK 足够实现 Driver，但官方仍标记 Beta；只有 Python 可定义 Modal Function，这不影响 Go 管理 Sandbox。[Go Sandbox SDK](https://modal.com/docs/sdk/go/latest/Sandbox)

可从 public registry 和私有 Docker Hub/ECR 等拉取 image，但只支持 `linux/amd64` 且 ENTRYPOINT 必须兼容。Modal 会把已拉取的外部 tag 当不可变缓存；AG 应要求 digest 或 immutable tag，不能依赖 `latest` 自动刷新。[Existing images](https://modal.com/docs/guide/existing-images)

command/args/env 可直接编译，Kubernetes Secret 可由 Controller 解析后构造临时 Modal Secret。第一版仍应限制一个主容器：Modal Sidecar 当前为 Alpha/Experimental，不宜声明稳定 MultiContainer capability。[Secrets](https://modal.com/docs/guide/secrets)

GPU 参数使用 `H100`、`A100-80GB:4` 等字符串，支持 fallback list 和单机多卡；Modal 是固定费率，Sandbox 没有 bid/max-price 参数，价格上限只能由 Controller/Driver 根据 billing rate 做前置判断。[GPU types](https://modal.com/docs/guide/gpu)、[Pricing](https://modal.com/pricing)

`Sandbox.exec` 支持 stdin/stdout/stderr、timeout、workdir、env、Secret 和 PTY，可直接实现 Exec；公开 API 没有 terminal resize，暂不声明 TTYResize。入口进程 stdout/stderr 可读，Python SDK 的 `sandbox.logs.fetch/tail` 可读历史 logs，但不含 exec logs 且当前不是 streaming；官方 CLI `modal container logs -f` 可 follow。[运行命令](https://modal.com/docs/guide/sandbox-spawn)、[Sandbox logs](https://modal.com/docs/sdk/py/latest/Sandbox)、[Container CLI](https://modal.com/docs/cli/latest/container)

创建时声明的端口可经 `Sandbox.tunnels()` 得到公网 URL，支持 TLS、非 TLS、HTTP/2 和 connection token。它可以成为 Endpoint，但不是私网任意 TCP 的 Kubernetes port-forward。[Sandbox networking](https://modal.com/docs/guide/sandbox-networking)

#### 硬盘和数据

Modal 有四条路径：

1. Sandbox root/local disk 是大容量临时空间，Sandbox 结束后不能视为持久；`emptyDir.sizeLimit` 也不能假定能精确满足。[CPU/memory/disk](https://modal.com/docs/guide/resources)
2. Modal Volume 是分布式持久文件系统，适合 write-once/read-many，支持 readonly/subpath 和 SDK/CLI upload/download。v1 有 commit/reload 和 final commit 语义，同一文件并发写是 last-writer-wins；v2 更适合 multi-writer，但仍为 Beta。[Volumes](https://modal.com/docs/guide/volumes)、[Volume CLI](https://modal.com/docs/cli/latest/volume)
3. CloudBucketMount 可挂 S3、R2、GCS，支持 read/write、prefix 和 read-only，但受 S3 Mountpoint 语义限制，不是完整 POSIX，更适合大文件顺序读取。[Cloud bucket mounts](https://modal.com/docs/guide/cloud-bucket-mounts)
4. Sandbox filesystem API 支持 local↔Sandbox copy/read/write/list/remove，Go 也覆盖，适合小文件 bootstrap，不适合 dataset 主路径。[Sandbox files](https://modal.com/docs/guide/sandbox-files)

Root filesystem 可 snapshot 成 Image 并启动新的 Sandbox，很适合 24 小时边界上的休眠/恢复，但不是持续挂载 volume。[Filesystem snapshots](https://modal.com/docs/guide/sandbox-snapshots)

| AGPI 方法 | Modal Driver |
|---|---|
| `ValidatePod` | 最强 Pod-like mapping；首版单容器、`linux/amd64`、最长 24h，校验 GPU/ports/volume |
| `EnsurePod` | named/tagged `Sandbox.create`，持久化 Sandbox ID |
| `GetPodStatus` | readiness + poll + exit code |
| `WatchPodStatus` | Driver polling |
| `DeletePod` | terminate，终态 no-op |
| `Logs` | 历史 logs 可取；实时能力需按 SDK 版本声明 |
| `Exec` / `Attach` | 原生 Exec + PTY；无 documented resize |
| `PortForward` | 代理预声明 tunnel；不能承诺任意私网端口 |

### AutoDL

#### 两套 REST API

AutoDL 并非没有 API。官方在 `https://api.autodl.com` 提供两套 REST API，认证 header 是原样 `Authorization: <token>`，不是 Bearer：

- **Container Instance Pro API** 面向完成个人或企业实名认证的用户，管理单实例；
- **Elastic Deployment API** 要求企业认证，支持 Container、Job 和 ReplicaSet。

[Container Instance Pro API](https://api.autodl.com/docs/instance_pro_api/)、[Elastic Deployment API](https://api.autodl.com/docs/esd_api_doc/)

Pro 提供 create/snapshot/status/list/power_on/power_off/release/save image/private image list。这里的 `snapshot` 是实例状态/详情快照，不是磁盘或 volume snapshot API。创建可选地区、1–4 GPU、GPU spec UUID、最低 CUDA、image UUID、0–500 GB system disk 扩容、remark name 和 `start_command`，当前 API create 只支持按量计费。`start_command` 失败不影响 instance running 状态，所以 Pod Ready/exit code 必须由 guest agent 报告。状态 snapshot 返回 price/resource/disk/image progress、SSH host/port/root password、Jupyter 以及 6006/6008 service URL。[Container Instance Pro API](https://api.autodl.com/docs/instance_pro_api/)

release 前官方要求先 power off，因此 DeletePod 是 `power_off → wait stopped → release`。

Elastic 支持 create/list/stop/delete Deployment、修改 ReplicaSet replica、query/stop container、query container events、query region GPU inventory，并能约束 GPU/CPU/memory/CUDA/price、映射 6006/6008、设置故障 host blacklist。Events 用 offset poll，不是 webhook/stream。[Elastic Deployment API](https://api.autodl.com/docs/esd_api_doc/)

一个 AGPod 应映射为 `deployment_type=Container`，避免把上层 AGJob/AGDeployment 的重试和副本状态机与 provider Job/ReplicaSet 叠加。建议拆成 `autodl-pro.anygpu.dev` 与 `autodl-elastic.anygpu.dev` 两个 Driver，后者提供企业增强 capability。

#### Image 是主要兼容障碍

AutoDL 官方明确不支持导入自定义 image，只能使用平台公共基础 image，或在 AutoDL instance 内配置环境后保存成 private `image_uuid`；instance 内也不支持 Docker。因此普通 `PodSpec.containers[].image` 无法直接执行。[Image](https://api.autodl.com/docs/image/)、[Instance environment](https://api.autodl.com/docs/env/)

Pro 只有一个 `start_command`，Elastic 只有 shell `cmd` 字符串，没有结构化 env、Secret、ConfigMap 或 multi-container 字段。`ValidatePod` 第一版应只接受单容器、已存在 `OCI digest → AutoDL image_uuid` 映射的 image；multi-container/init 应拒绝，imagePullSecrets 只有在完成 image 映射后才能忽略。按照 AG 的兼容策略，无法落实的 `securityContext` 字段只返回 warning 并在 status 中持续标记 `Degraded`，不阻止调度；调用方必须能看到 `runAsNonRoot`、`readOnlyRootFilesystem`、`allowPrivilegeEscalation: false` 等保证实际没有生效。影响数据正确性的 volume、PVC 和 mount 语义则必须报 error，不能降级为忽略。

实际可行方案是维护固定 `anygpu-guest`/`rc-kube` AutoDL private image，由 API 启动 guest shim，再经 SSH/mTLS 投递解析后的 Pod bundle。guest 负责 projection、env、启动/监控主进程、exit code、logs 和 graceful stop。官方也建议 SSH 长任务用 screen/tmux 并重定向日志，说明简单远程 shell 生命周期不足以模拟 Pod。[后台任务](https://api.autodl.com/docs/daemon/)

#### 状态、幂等、交互和端口

Pro 只有 status/snapshot polling；Elastic 可 offset-poll container events，但仍需周期性全量 list 防止 cursor 丢失或 Driver restart。公开 API 没有 idempotency key、label/tag，也未承诺 name 唯一。Driver 必须用 durable operation journal 对 AG UID 唯一约束 create intent；不确定时按确定性 name/time/spec 查找并 adopt，多余资源先 quarantine 再 GC。

控制 API 没有 exec/log endpoint；Pro snapshot 和 Elastic container list 会返回 SSH command/root password，账户控制台可设置 SSH public key，使新建或重启 instance 支持 key auth。Driver 应使用账户 SSH key，Exec 用 SSH channel，Logs 用 guest protocol 或 `tail -F`，不把 root password 当长期控制面。[SSH](https://api.autodl.com/docs/ssh/)

Instance 没有独立 public IP，只固定映射 6006 和 6008，可选择 HTTP/TCP，开放服务要求企业认证；任意远端 port 可通过 SSH local forwarding。于是 AGService 的原生公网 endpoint 只能覆盖两个端口，AGPI PortForward 可走 SSH `direct-tcpip`。[Ports](https://api.autodl.com/docs/port/)、[SSH tunnel](https://api.autodl.com/docs/ssh_proxy/)

#### 硬盘和数据

AutoDL 各存储路径语义不同：

- `/` system disk 会被 private image save 捕获；换 image 清空 system disk，但不影响 data disk。[Images](https://api.autodl.com/docs/image/)
- `/root/autodl-tmp` 是高性能 local SSD、无冗余，一般默认 50 GB，power off 保留，release 后消失，不能保存进 image。[Local disk](https://api.autodl.com/docs/local_disk/)、[目录说明](https://api.autodl.com/docs/env/)
- `/root/autodl-fs` 是同 region 多实例共享、多副本文件存储，不随 instance release，I/O 较慢；必须先在 console 按 region 初始化，官方 API 没有 create file-storage interface。[File storage](https://api.autodl.com/docs/fs/)
- `/root/autodl-nas` 按 region 独立、实例间共享并在 release 后保留，性能低于 data disk；跨 region 需 SCP。[NAS](https://api.autodl.com/docs/nas/)
- `/root/autodl-pub` 是只读公共 dataset；SCP/Jupyter/web file storage 可用于传输。[上传数据](https://api.autodl.com/docs/scp/)

本地 system/data disk 通常无冗余，连续关机 15 天会自动释放 instance 并清空数据。[数据保留](https://api.autodl.com/docs/instance_data/) File storage 是 region-local；绑定它的 AGPod 必须把 placement 收窄为单 region。Elastic 的 `reuse_container=true` 会留下旧 container data，为满足 `emptyDir`/Pod isolation，默认必须为 false；若显式开启，guest 必须清理 UID 专属目录。[Elastic best practices](https://api.autodl.com/docs/elastic_deploy_practice/)

官方 docs 没有呈现官方 SDK/CLI，主要提供 REST 与 Python `requests` 示例。Go Driver 应写小型 typed HTTP client，并验证 provider response envelope，而不只看 HTTP 2xx。

| AGPI 方法 | AutoDL Pro | AutoDL Elastic |
|---|---|---|
| `ValidatePod` | 单容器、image UUID mapping；securityContext 仅 warning，错误 volume 语义拒绝 | 同左，另可做 GPU/region/price inventory check |
| `EnsurePod` | Pro create | `deployment_type=Container` |
| `GetPodStatus` | status/snapshot + guest status | container list/events + guest status |
| `WatchPodStatus` | Driver polling | offset events + periodic full reconcile |
| `DeletePod` | power off→wait→release | stop/delete deployment |
| `Logs` | guest log/SSH tail | guest log/SSH tail |
| `Exec` / `Attach` | SSH/guest | SSH/guest |
| `PortForward` | SSH forwarding | SSH forwarding；公网仅 6006/6008 |

### “算了么”与共绩算力

#### 产品身份

用户所说的“算了么”可以明确对应 `suanleme.cn`，不是同音的未知厂商。算了么的官方联系页将“购买算力”直接导向共绩算力；共绩的隐私协议又把“共绩云服务、算了么客户端、计算任务接入/分发服务、Serverless 服务、Docker 服务”列为同一套共绩服务。因此，对需求侧 AG Driver 有意义的接口是现行 **共绩算力 Open API**，而不是用于贡献闲置 Windows GPU 的算了么客户端。[算了么联系页](https://suanleme.cn/docs/contact.html)、[共绩隐私协议](https://www.suanli.cn/docs/platform/privacy-policy/twpmwbmy2iiarbksnvccd35ontd/)

算了么客户端仍说明了供给侧特征：当前客户端面向 Windows/NVIDIA 单卡，使用 WSL2/Docker 拉取镜像，一个设备同时运行一个任务点，客户端离线超过约三分钟会释放任务点；它是 GUI agent，不是面向租用方的 CLI。[客户端详细指南](https://suanleme.cn/docs/app/detail.html)、[客户端 FAQ](https://suanleme.cn/docs/app/faq.html)

#### API 与认证

共绩公开的是 HTTPS/JSON REST API，base URL 为 `https://openapi.suanli.cn`。所有接口带独立 `version`、毫秒 `timestamp` 和 `token` header；生产可使用 RSA-SHA256/PKCS1v15 的 `sign_str`，待签名内容为 URI、版本、时间戳、token 和请求体。涉及对象存储凭据或 Job task DTO 的接口还要求请求体加密。官方文档提供 cURL、Python、Java、Go 等请求示例，但未发现独立的官方 CLI 或语言 SDK；因此 Driver 应直接实现 HTTP client 和签名/加密层。[Open API 使用文档](https://suanli.cn/docs/platform/openapi/zx3iwhbv1i8sxdkeiapcprxhn8d/)、[RSA 模式指南](https://www.comnergy.com/docs/platform/openapi/m3p6whioxidzwaksughc4gfhnro/)

#### 资源、生命周期与状态

资源发现接口 `GET /api/deployment/resource/search` 按任务类型和设备类型查询，返回 GPU 型号、数量、显存、CPU、内存、系统盘、区域、库存、价格以及一个创建任务时必须原样回传的 `mark`。这非常适合实现 Driver 的实时 `ValidatePod` 和 placement，但不能把 `mark` 当长期稳定的机型 ID。[资源列表接口](https://s.apifox.cn/6aa360d3-d8f2-471e-b841-3a35c33a7b7c)

弹性部署 API 支持 list/detail/create/modify/recover/pause/delete 和修改节点数。创建请求可提交一个或多个容器的 image、command/args、env、端口、HTTP health probes、init services、资源权重和多类 storage mount；详情返回 `Pending`/`Running` 等任务状态、计费节点数和公开 URL。删除调用 `POST /api/deployment/task/stop`，官方明确说明会释放资源且不可恢复。[弹性任务创建](https://s.apifox.cn/6aa360d3-d8f2-471e-b841-3a35c33a7b7c/api-434943737)、[任务详情](https://s.apifox.cn/6aa360d3-d8f2-471e-b841-3a35c33a7b7c/api-433641836)、[恢复任务](https://s.apifox.cn/6aa360d3-d8f2-471e-b841-3a35c33a7b7c/api-296882718)、[删除任务](https://s.apifox.cn/6aa360d3-d8f2-471e-b841-3a35c33a7b7c/api-314631776)

一次性任务可以映射到 Job 队列：先创建 queue，配置 Pod 并行上限和任务组超时，再通过加密的 `POST /api/job/queue/encrypt/push` 推送任务组。task DTO 原生包含 Spot、GPU/region 候选及权重、`parallelism`、timeout、backoff/restart policy、termination grace period、init/regular containers、probes、端口和存储挂载；group detail 返回 Pending/Running/Paused/End 计数。[Job queue 创建](https://s.apifox.cn/6aa360d3-d8f2-471e-b841-3a35c33a7b7c/api-469630957)、[推送任务组](https://s.apifox.cn/6aa360d3-d8f2-471e-b841-3a35c33a7b7c/api-469643556)、[任务组详情](https://s.apifox.cn/6aa360d3-d8f2-471e-b841-3a35c33a7b7c/api-469644479)

若 AGPod 借用共绩 Job 作为一次性 container transport，必须把 provider parallelism 固定为 1、backoff 固定为 0，并由 AGJob 独占 retry/completion 语义；否则会出现两层 Job controller 同时补任务。

状态侧有任务详情、节点列表、节点日志和节点事件的轮询接口。日志是一次返回字符串，不是 documented streaming API；事件返回 reason、message、type、time 和 service。官方未公开 webhook 或 watch stream，所以 `WatchPodStatus` 应由 Driver 用 list/detail/events 做增量轮询并自行去重。[节点列表](https://s.apifox.cn/6aa360d3-d8f2-471e-b841-3a35c33a7b7c/api-296885186)、[节点日志](https://s.apifox.cn/6aa360d3-d8f2-471e-b841-3a35c33a7b7c/api-335612564)、[节点事件](https://s.apifox.cn/6aa360d3-d8f2-471e-b841-3a35c33a7b7c/api-335613302)

创建返回平台 `task_id`/`group_id`，请求可携带 task name 和 task tags；公开 schema 没有显式 client request token。`EnsurePod` 因此要先按不可冲突的 AG UID name/tag 查找，再创建并立刻持久化平台 ID，不能假定重试 create 天然幂等。[弹性任务创建](https://s.apifox.cn/6aa360d3-d8f2-471e-b841-3a35c33a7b7c/api-296881505)、[Job 推送 schema](https://s.apifox.cn/6aa360d3-d8f2-471e-b841-3a35c33a7b7c/api-469643556)

#### 网络、交互与 PodSpec 映射

弹性服务可以为容器端口返回 HTTPS URL；多节点服务默认经过轮询负载均衡。API 可取日志和事件，平台 UI 有 Web shell，但弹性部署明确不支持 SSH，且网页 shell 断开后不能恢复原先前台进程。本次未找到公开的 exec、attach 或任意 TCP port-forward API。因此 `Logs` 可实现，`Exec`/`Attach`/`PortForward` 不能作为共绩 Driver 的基础 capability；rc 若依赖交互控制，应让 `rc-kube` 主动建立出站控制通道，而不是依赖平台 Web shell。[弹性部署限制](https://suanli.cn/docs/flexible-deployment/product-brief-introduction/hhadwmdblixrydkrogrcwqrjnme/)、[任务详情的 endpoint](https://s.apifox.cn/6aa360d3-d8f2-471e-b841-3a35c33a7b7c/api-433641836)

共绩支持多容器和 init container，Pod 字段映射面比一般租机 API 丰富；但是公开 API 是平台 task DTO，不是 Kubernetes Pod API。Driver 仍须逐字段编译 PodSpec，并对 ServiceAccount token、Kubernetes DNS、host namespace、PVC 等不能等价满足的字段报 `Unsupported`。

#### 硬盘和数据

平台提供三条数据路径：

- 本地系统盘/数据盘是节点本地 SSD，官方明确提示无冗余、单点故障可能永久丢失，不能作为 rc Workspace 的权威副本。[云主机数据安全说明](https://suanli.cn/docs/cloud-hosting/best-practice/k6bcwuuhaiudqykruwtcdal6nxb/)
- 共享存储卷可挂到指定目录，实例销毁后数据仍在，多实例和跨区域可共享，但文档说明多点写入可能有短暂同步延迟、最终一致；当前单卷最大 200 GB，最多五个 bucket。它适合作为 checkpoint、工具 cache 或小型 Workspace home，不应假定具有 Kubernetes RWX PVC 的严格一致性。[共享存储卷说明](https://suanli.cn/docs/storage-service/shared-storage-volume/u5d3wiyetiazdckqxs7cvtuxndc/)
- 对象存储加速接入用户自己的 S3-compatible bucket，以 JuiceFS/region cache 挂到容器目录；源对象存储才是权威数据，加速区域可预热。Open API 可创建、激活、浏览、预热和释放配置。它是五个平台中很好的可移植数据路径，但应避免让每个 Pod request 都长期携带 AK/SK。[对象存储加速说明](https://suanli.cn/docs/flexible-deployment/function-usage-instructions/wqhqwbcf3i6byykijbvct9dkn0e/)、[创建对象存储配置](https://s.apifox.cn/6aa360d3-d8f2-471e-b841-3a35c33a7b7c/api-478747309)、[预热接口](https://s.apifox.cn/6aa360d3-d8f2-471e-b841-3a35c33a7b7c/api-478747312)

#### AGPI 适配判断

| AGPI 方法 | 共绩 Driver |
|---|---|
| `ValidatePod` | 强：资源搜索可校验 GPU/region/inventory，task schema 可覆盖 image、command、env、init container、probe、port、volume；仍须拒绝 Kubernetes-only 语义 |
| `EnsurePod` | 强：长服务用 Deployment task，一次性 Pod 用 Job task/group；用 AG UID name/tag 实现查找后创建 |
| `GetPodStatus` | 强：task/group/node detail 映射状态 |
| `WatchPodStatus` | 中：只有轮询和事件列表，没有公开 watch/webhook |
| `DeletePod` | 强：pause、stop/delete、单节点删除均有接口 |
| `Logs` | 中：有按 task/point/service 查询的非流式日志 |
| `Exec` / `Attach` | 弱：只有 UI Web shell，没有公开 API |
| `PortForward` | 弱：只适合已注册端口和平台 HTTPS endpoint，不是任意端口隧道 |

## 对 AGPI 的统一设计建议

### 1. 公共接口围绕 desired state，不围绕虚拟机动作

Provider 之间稳定的最小公分母仍然是：

```protobuf
service PodRuntime {
  rpc ValidatePod(ValidatePodRequest) returns (ValidatePodResponse);
  rpc EnsurePod(EnsurePodRequest) returns (EnsurePodResponse);
  rpc GetPodStatus(GetPodStatusRequest) returns (GetPodStatusResponse);
  rpc WatchPodStatus(WatchPodStatusRequest) returns (stream PodStatusEvent);
  rpc DeletePod(DeletePodRequest) returns (DeletePodResponse);
}
```

`EnsurePod` 的输入带完整、不可变的 `core/v1 Pod` JSON、Pod UID、generation、spec digest 和解析后的 Secret/ConfigMap。输出至少含：

```text
providerID       provider 对象的稳定 ID
providerClass    实际选用的 provider/region/offer
observedDigest   Driver 已实现的 Pod spec digest
endpoints[]      可访问的 protocol/host/port/URL
diagnostics[]    ignored/degraded/unsupported 字段
```

不要在协议中出现统一的 `StartVM`、`CreateFunction` 或 `CreateTaskGroup`。这些是 Driver 内部 execution plan 的动作。

### 2. capability 分为声明、校验和运行时降级三层

Identity service 可以粗粒度声明 `MultiContainer`、`InitContainers`、`Exec`、`StreamingLogs`、`PublicHTTP`、`TCPPorts`、`PersistentVolume`、`ObjectStorageMount` 等 capability，并返回 `maxRunDuration` 这类硬边界；`ValidatePod` 再对具体 Pod 字段组合给出诊断。库存、价格和区域随时变化，不属于静态 capability，必须在 ensure 前重新校验。

`AGJob`、`AGService` 和 rc Workspace 不应假定同一 duration contract。Modal Sandbox 的 24 小时上限对 Job 可以接受，但对长期 Workspace 意味着必然 rollover；底层 Sandbox 重建时，活着的进程不能无损迁移。rc 必须把这种 runtime replacement 按现有语义标为进程 lost，或明确把 Modal 限定为有截止时间的 Workspace。

`WatchPodStatus` 应是 AG Driver 对 Controller 的稳定 streaming contract，不要求底层供应商一定有 watch。只有轮询 API 的 Driver 可以在内部 poll、退避、去重并生成单调递增的 event sequence。

### 3. 幂等不能寄托在供应商 create API

所有 Driver 使用同一套恢复协议：

1. 以 `AGPod.metadata.uid` 作为唯一 idempotency key；
2. 能加 label/tag 的平台写 `anygpu.dev/pod-uid`，`EnsurePod` 先按 UID 查找再 create；
3. 无 tag、unique name 或 provider idempotency key 的平台，在外部调用前写 durable create intent；响应不确定时全量 list 做弱恢复，无法唯一识别就进入 `UnknownCreate`，不能盲目再次 create；
4. create 返回后立即把 `providerID` 写入 Driver durable state 和 AGPod status；
5. Controller/Driver 重启时同时用 providerID、ownership tag 和 operation journal 反查；
6. `DeletePod` 对 not-found 返回成功；
7. 单独运行 orphan collector，但只有能以强 ownership tag 或 journal 证明属于 AG 的资源才允许自动删除；仅 name/time 启发式匹配的对象先 quarantine。

### 4. 交互能力不应成为所有 Provider 的准入门槛

`Exec`、`Attach`、`Logs` 和 `PortForward` 应是 optional streaming services。对 rc 这类必须长期交互的 workload，更可移植的做法是在 runner image 内放 `rc-kube` agent，由它通过 HTTPS/WebSocket/gRPC 主动连接 AG gateway：

```text
rcctl -> AG gateway <- outbound mTLS/WebSocket <- rc-kube in remote container
```

这样只要求 provider 允许容器出站 HTTPS，不要求每家都公开 SSH/exec/port-forward API。供应商原生 logs/exec 仍可用于 bootstrap 诊断和 fallback。

## 硬盘与数据平面设计

### 1. 明确四种 durability class

| 类别 | PodSpec 表示 | 生命周期 | 适用内容 |
|---|---|---|---|
| Image layer | `containers[].image` | 随 image digest | OS、依赖、`rc-kube`，不可写状态 |
| Ephemeral scratch | `emptyDir` | 随 AGPod | `/tmp`、解压、编译中间物 |
| Provider volume | PVC/AG volume binding | 跨 AGPod，但绑定 provider/region | 热缓存、active Workspace、低延迟 checkpoint |
| Portable artifact store | AG data binding / object URI | 跨 provider | repo bundle、dataset、model、checkpoint、最终结果 |

必须在 status 中记录每个 mount 的 durability、真实后端、region 和最后同步点。不能把 Vast 本地盘、RunPod container disk 或某厂商 volume 都伪装成语义相同的 Kubernetes PVC。

### 2. 保留 PodSpec，并在其旁边增加解析后的 volume binding

用户仍然写标准 `PodSpec.volumes`/`volumeMounts`。AG Controller 先解析 PVC、Secret、ConfigMap 和 AnyGPU 的数据资源，再给 Driver 一个明确 binding：

```protobuf
message VolumeBinding {
  string volume_name = 1;
  string mount_path = 2;
  bool read_only = 3;
  oneof source {
    EmptyDir empty_dir = 4;
    MaterializedFiles files = 5;
    ProviderVolume provider_volume = 6;
    ObjectStore object_store = 7;
    TransferPlan transfer = 8;
  }
}
```

这里的 binding 是 Controller 解析结果，不替代 PodSpec，也不要求 Driver 拥有 Kubernetes API 权限。普通集群 PVC 只有在存在明确的数据导出/复制方案时才能编译成 `TransferPlan`；否则 `ValidatePod` 返回 `UnsupportedVolumeSource`。

### 3. provider-native volume 由独立的 Volume Driver 管理

AGPI 只消费一个已经解析的 `provider_volume.handle`。创建、扩容、快照、删除和 region 约束放到伴随协议（可命名为 AGVI）中：

```protobuf
service VolumeRuntime {
  rpc EnsureVolume(EnsureVolumeRequest) returns (EnsureVolumeResponse);
  rpc GetVolumeStatus(GetVolumeStatusRequest) returns (GetVolumeStatusResponse);
  rpc SnapshotVolume(SnapshotVolumeRequest) returns (SnapshotVolumeResponse);
  rpc DeleteVolume(DeleteVolumeRequest) returns (DeleteVolumeResponse);
}
```

它可以由同一个 provider Driver 进程实现，但协议和 capability 与 PodRuntime 分开。原因是 volume 通常比 Pod 活得久，而且删除错误的收费和数据风险都更高。

### 4. 对象存储是跨 provider 的交换层

推荐定义 `AGDataSource`/`AGDataSink`，后端先支持 S3-compatible URI：

```yaml
spec:
  source: s3://rc-artifacts/workspaces/ws-123/checkpoints/42
  targetPath: /home/agent
  mode: ReadWriteCheckpointed
  sync:
    restoreOnStart: true
    checkpointEvery: 60s
    flushOnTermination: true
```

执行方式按 provider capability 选择：

1. 供应商原生对象存储 mount/cache；
2. init/sidecar data mover；
3. `rc-kube` 内置增量 checkpoint agent。

传输凭据由 Controller 解析为短期、最小权限凭据；Driver 不应获得 namespace-wide Secret list 权限。大 dataset 应使用 manifest + content digest 和并发/断点续传，避免每次启动重新打 tar 包。

### 5. Mount 与 Stage/Collect 必须是两种显式语义

同一个 S3 URI 既可能被 provider 用 FUSE/Mountpoint 挂载，也可能先复制到 local SSD；两者的性能、失败和一致性完全不同，不能只用一个 `volumeMount` 隐藏：

| 模式 | 开始前 | 运行中 | 结束时 | 适用场景 |
|---|---|---|---|---|
| `Mount` | 建立 provider volume 或 object filesystem mount | 直接读写后端；一致性由后端决定 | unmount/flush | 小文件交互、共享 Workspace、持续 checkpoint |
| `Stage` + `Collect` | 按 manifest/digest 将输入复制到 local scratch | 使用本地 SSD；远端不会自动看到变化 | 按 `OnSuccess`/`Always` policy 上传输出 | 大型 dataset、训练、批处理 |
| `Stage` + periodic checkpoint | 同上 | 本地运行，定时上传 generation checkpoint | 尽力 final flush | Spot/可抢占任务、可迁移 Workspace |

建议把 Pod data lifecycle 公开成 Conditions，而不是让 Pod 长时间停在含糊的 `Pending`：

```text
DataScheduled → DataPreparing → DataReady → Running
Running → DataFinalizing → DataCommitted → Succeeded
```

`DataReady` 有两种到达方式。若 provider 允许在分配计算前预热 native volume/object cache，Driver 应先完成 pre-stage 再创建收费 GPU；Vast、AutoDL 等需要先拿到运行中实例才能访问 local disk 时，只能先进入 `ComputeAllocated=True, DataPreparing=True`，再由 in-guest mover stage。状态和计费观察必须暴露这段时间，不能假装 GPU 尚未分配。Pod 被主动删除时，`terminationGracePeriodSeconds` 必须给 final collect 留时间；Spot/host failure 不能保证结束时 collect，因此关键输出必须周期 checkpoint。

`writeBackPolicy` 至少应有 `Never`、`OnSuccess`、`Always`、`Periodic`。每次提交生成 immutable manifest，包含 source generation、object digest、size 和完成标记；没有完成标记的 upload 不能成为下一次 restore 的权威版本。

### 6. rc Workspace 的 PVC 迁移约束与落地顺序

rc 当前把 Workspace home、Worktree 和 Environment 建立在可克隆 PVC 上，并要求 active Workspace 对这些目录提供普通 POSIX 读写。[Workspace runtime design](../design/workspace-runtime.md) Kubernetes PVC 的 volume handle 只对相应 CSI backend/topology 有意义；远端 GPU provider 看不到集群 Node 的 block device 或 mount namespace。因此：

- **同一个外部 backend 可从两边访问**：可以把对象存储/NFS 解释成新的共享数据源，但它仍不是“把现有 PVC 挂过去”。
- **CSI backend 不能从 provider 访问**：必须 snapshot/export 到 object storage，再 stage/restore；若 StorageClass 不支持快照，还需要在 Kubernetes Pod 中挂 PVC 做文件级 data mover。
- **provider-native volume**：只能留在该 provider/topology，迁移时先 checkpoint 到可移植 store，再在目标 provider 创建新 volume 并 restore。
- **RWO active Workspace**：迁移必须先 fencing 旧 writer、完成 checkpoint、写 commit record，再启动新 writer；不能让两个 Driver 根据 eventual status 同时认为自己拥有写租约。

建议分三阶段：

1. **阶段 A：无持久状态的 AGJob**。image + command/env，输入从 object store 下载，输出上传；先验证所有 Driver 的 lifecycle/status/logs。
2. **阶段 B：固定 provider 的持久 Workspace**。使用该 provider 的原生 volume，AGPod 与 AGVolume 做 provider/region affinity；rc-kube 用主动控制通道实现 attach/exec。
3. **阶段 C：可迁移 Workspace**。active volume 是热工作副本，`rc-kube` 周期性向 object store 写带 generation 和 digest 的 checkpoint；只有成功 flush 并写入 commit record 后才允许另一 provider restore。不要尝试双写两个 provider volume。

对于 git worktree，Git remote + object store bundle 可以承担可恢复源；对于 `/home/agent` 的大量小文件，建议 block/file-level 增量快照，而不是频繁全目录对象同步。迁移期间把 Workspace condition 置为 `Checkpointing`/`Restoring`，用 fencing token 防止两个 AGPod 同时写同一逻辑 Workspace。

## 推荐的首版支持矩阵

`原生` 表示官方控制面直接提供；`适配` 表示 Driver 可用 polling、SSH 或 AG guest agent 实现；`有限` 表示只覆盖该 provider 预先声明的端口/存储语义。“适配难度”是本报告基于四项工程量的主观估算：稳定控制面、OCI/command 映射、幂等恢复以及 exec/log/data 能力，不代表平台成熟度或服务质量。

| 能力 | RunPod | Vast.ai | Modal | AutoDL Pro | AutoDL Elastic | 共绩算力 |
|---|---|---|---|---|---|---|
| 推荐控制面 | REST v2 | REST v0/v1 | Go/Python SDK | REST | REST | REST |
| 主要执行对象 | 单容器 Pod | market instance | Sandbox | instance | Container deployment | Deployment task / Job group |
| 任意 OCI image | 原生 | 原生 | 原生，`linux/amd64` | **否；需 image UUID 映射** | **否；需 image UUID 映射** | 原生 registry image |
| 结构化 command/env | 有限；entrypoint args/env | 有限；runtype/onstart/args/env | 原生 | 仅 shell start command | 仅 shell cmd | 原生 task DTO |
| MultiContainer / init | 否 | 否 | 首版否；sidecar experimental | 否 | 否 | 原生 task DTO |
| GPU/region/inventory | 原生 Catalog | 原生 offer search | 原生 GPU/region | 原生 spec/region | 原生，含 price filters | 原生 resource search/mark |
| Spot / 抢占式 | v2 暂无 | 原生 bid | 无 bid | API create 暂无 | 官方有 Spot/price model | Job DTO 有 Spot |
| 状态 watch | 适配：poll | 适配：poll | 适配：poll | 适配：poll | 适配：offset events + poll | 适配：events + poll |
| Logs follow | **原生 SSE** | 适配：snapshot/SSH/agent | 有限：SDK/CLI 能力不同 | 适配：SSH/agent | 适配：SSH/agent | 有限：非流式 API |
| Exec / PTY | 适配：SSH/agent | 非交互 API；PTY 用 SSH | **原生 Exec/PTY** | 适配：SSH/agent | 适配：SSH/agent | 无公开 API；需 agent |
| 任意 PortForward | 适配：SSH | 适配：SSH | 有限：预声明 Tunnel | 适配：SSH | 适配：SSH | 无公开 API；只可用 endpoint/agent |
| Provider persistent volume | data-center Network Volume | host-local Volume | Modal Volume | region FS/NAS，需 console 初始化 | region FS/NAS | shared volume / NAS |
| Object storage 路径 | Network Volume S3 API；外部 S3 更可移植 | Cloud Copy/Sync 到外部 store | CloudBucketMount | guest stage/collect | guest stage/collect | S3-compatible JuiceFS mount/cache |
| 首版 AGJob/AGPod 适配难度 | 低 | 中 | **最低** | 高 | 中高 | 中 |

建议首版按以下顺序实现：

1. Modal Sandbox：最完整地验证 Pod lifecycle、Exec、Secret 和 volume binding；
2. RunPod REST v2：验证 GPU Pod、SSE logs 和 data-center volume；
3. Vast.ai：验证两阶段 marketplace saga、bid interruption 与 host-affine volume；
4. 共绩算力：验证国内 REST task/Job、multi-container compilation 与 object-store mount；
5. AutoDL Elastic/Pro：最后处理 image UUID build pipeline 和 guest shim，这部分不是简单 API adapter。

### 每个 Provider 的推荐数据组合

| Provider | 热工作盘 | 可移植数据 | 不应保存权威数据的位置 |
|---|---|---|---|
| RunPod | Secure Cloud Network Volume | 外部 S3；支持地区也可经 volume S3 API 导入导出 | container disk、随 Pod 删除的 pod volume |
| Vast.ai | host-affine Volume | 外部 S3/Backblaze/Drive，经 Cloud Sync 或 agent | instance storage；单独 host Volume 也必须有备份 |
| Modal | Modal Volume；24h 边界可 snapshot Sandbox root filesystem 后重建 | S3/R2/GCS CloudBucketMount 或 agent checkpoint | Sandbox local filesystem |
| AutoDL | 同 region `/root/autodl-fs`/NAS；计算时 local SSD | guest 从 S3/OSS stage/collect | `/root/autodl-tmp` 和 system disk |
| 共绩算力 | shared volume/NAS | 用户 S3-compatible bucket + JuiceFS cache | 无冗余 local system/data disk |

### 数据安全边界

把 Secret 和源代码发到外部 provider 是安全域迁移，不是普通 scheduling。AGProviderClass 至少应声明 provider/host trust tier、数据驻留 region、是否允许 community/consumer host、network egress policy 和可使用的 Secret classes。Object-store credential 应为短期、限定 prefix、限定 read/write action 的凭据；Secret/ConfigMap projection 应通过 mTLS guest channel 或 provider secret object 注入，不能拼到 shell command、resource name、event 或 status。

尤其对 marketplace 或闲置消费级设备，AG 默认策略应拒绝携带生产 credential 的 Pod，除非用户显式选择相应 trust class。磁盘静态加密不能防止正在执行 workload 的 host operator 读取明文，敏感任务需要可信执行环境或合同/专属资源，而不是只增加一层上传加密。

## 调查方法

调查优先读取各平台官方 API、SDK/CLI 源码和存储文档，再把明确公开的操作映射到 AGPI。没有用第三方教程补齐厂商未承诺的接口；设计建议中的推断均与厂商事实分开书写。
