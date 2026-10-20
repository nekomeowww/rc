# HAMi DRA 与 cert-manager canary 记录

调查日期：2026-10-04

## 背景

本次实验在 ihome Kubernetes `v1.36.3` 集群的 `neko-gpu-1` 上进行。节点使用 RTX 4080 SUPER、NVIDIA Driver `610.57.04`、containerd `2.2.6`。实验目标是确认 Project HAMi 的 DRA driver 能否替代该节点原有的 NVIDIA DRA driver，并验证 rc Workspace、CUDA 和 PyTorch 工作负载的 vGPU 行为。

`liet-gpu-1` 继续使用 NVIDIA DRA，作为未迁移节点。`neko-gpu-1` 上原有的 GPU Workspace `minecraft-spatial-3090` 保持 `Suspended`，存储未删除。

## 当前部署

- cert-manager chart：`v1.21.2`
- HAMi-DRA chart：`0.2.3`
- HAMi DRA driver：`ghcr.io/project-hami/k8s-dra-driver:v0.1.1`
- DeviceClass：`hami-core-gpu.project-hami.io`
- 节点标签：`gpu=on`、`hami.project-hami.io/dra-kubelet-plugin=true`
- cert-manager 私钥轮换策略：`Always`

为避免 HAMi webhook 接管仍由 `liet-gpu-1` 承载的 `nvidia.com/gpu` 请求，canary 使用独立资源名：

```text
canary.hami.ihome.cat/gpu
canary.hami.ihome.cat/gpumem
canary.hami.ihome.cat/gpucores
```

节点运行时配置已满足 HAMi 的前置条件：containerd 默认 runtime 为 `nvidia`，`accept-nvidia-visible-devices-as-volume-mounts=true`。修改前的 NVIDIA Container Runtime 配置保存在节点的 `/etc/nvidia-container-runtime/config.toml.pre-hami-20261004`。

## 自签证书模式下的 caBundle 问题

### 观察

第一次安装 HAMi-DRA 时使用了以下模式：

```yaml
certs:
  certManager:
    enabled: false
  custom:
    crt: <PEM certificate>
    key: <PEM private key>
```

chart 正确创建了 TLS Secret，webhook Pod 也加载了对应证书。但是生成的 `MutatingWebhookConfiguration` 和 `ValidatingWebhookConfiguration` 中没有 `clientConfig.caBundle`。

API Server 创建 Pod 时因此拒绝调用 webhook：

```text
failed calling webhook "mutate.hami.io":
tls: failed to verify certificate: x509: certificate signed by unknown authority
```

将同一张证书写入两个 WebhookConfiguration 的 `clientConfig.caBundle` 后，server-side dry-run 和实际 Pod 创建立即恢复。

### 代码位置

HAMi-DRA `0.2.3` 的 webhook templates 只在 `certs.certManager.enabled=true` 时添加 `cert-manager.io/inject-ca-from` annotation。关闭 cert-manager 后，chart 会创建包含自定义证书的 Secret，但没有把证书内容渲染为 `caBundle`。

涉及的 templates：

- `charts/hami-dra/templates/webhook/mutatingwebhookconfiguration.yaml`
- `charts/hami-dra/templates/webhook/validatingwebhookconfiguration.yaml`
- `charts/hami-dra/templates/webhook/cert-secret.yaml`

### 预期行为

当 `certs.certManager.enabled=false` 且提供 `certs.custom.crt/key` 时，chart 至少应满足下列一种行为：

1. 把可信 CA 写入两个 WebhookConfiguration 的 `clientConfig.caBundle`；
2. 提供独立的 `certs.custom.caBundle` value，并在缺失时让 Helm rendering 失败；
3. 明确把「关闭 cert-manager」标记为需要用户另外 patch caBundle 的安装模式。

当前 README 只要求提供自定义证书，没有说明还要手动维护 WebhookConfiguration，因此更接近 chart 缺陷。后续可以据此向 Project-HAMi/HAMi-DRA 提交 Issue。

## cert-manager 重装验证

安装 cert-manager 后，HAMi-DRA 使用默认的 Issuer、Certificate 和 cainjector 路径重新安装。验证结果：

- `Issuer/hami-dra-dra-webhook-selfsigned-issuer` Ready；
- `Certificate/hami-dra-dra-webhook-serving-cert` Ready；
- CertificateRequest Approved 且 Ready；
- TLS Secret 中的 `ca.crt` 与两个 WebhookConfiguration 的 `caBundle` SHA-256 指纹一致；
- server-side dry-run 创建 Pod 成功；
- webhook Deployment 滚动后仍能正常创建 DRA ResourceClaim。

这条路径不需要手工 patch caBundle，并能由 cert-manager 处理后续续期和私钥轮换。

## vGPU 实验结果

### CPU 工作负载

并行运行 8 个各请求 1 CPU 的 BusyBox Job。全部完成，普通 CPU 容器中没有 `/dev/nvidia*` 设备，说明将 containerd 默认 runtime 设为 `nvidia` 没有把 GPU 自动暴露给普通 Pod。

### CUDA 13 工作负载

使用 `ghcr.io/nekomeowww/oci-ml-nvidia-cuda:py3.13-13.0` 同时运行两个 Pod。每个 Pod 请求：

```text
GPU count: 1
GPU memory: 4096 MiB
GPU cores: 50
```

两个容器都观察到 RTX 4080 SUPER 和 `4096 MiB` 可见显存，HAMi-core 初始化成功，`cuInit(0)` 返回成功。

### PyTorch 并发

使用 `ghcr.io/nekomeowww/oci-ml-pytorch:py3.13-cu13.0-2.9.0` 同时运行两个 Pod。每个 Pod 请求 6144 MiB 和 50% GPU cores，并在约 49 秒内持续执行 `4096 x 4096` 矩阵运算：

| Pod | iterations | allocated bytes | peak bytes | exit code |
| --- | ---: | ---: | ---: | ---: |
| `pytorch-share-n54w5` | 4797 | 3296854016 | 3363964928 | 0 |
| `pytorch-share-xrvbm` | 5248 | 3296854016 | 3363964928 | 0 |

两个 Pod 的执行时间重叠，证明同一块物理 GPU 能同时服务两份 HAMi DRA claim。

在两个 4096 MiB CUDA claim 尚未释放时，两个 6144 MiB PyTorch claim 保持 Pending；CUDA Job 完成后，PyTorch claim 自动获得分配。这个过程验证了 DRA consumable capacity 的调度账本。

### 显存越界

给 PyTorch Pod 分配 2048 MiB 后尝试申请 3 GiB。容器只看到 2048 MiB，HAMi-core 拒绝分配并向 PyTorch 返回 CUDA OOM：

```text
Device 0 OOM 3481272320 / 2147483648
CUDA out of memory. Tried to allocate 3.00 GiB.
```

### rc Workspace

使用 `rcctl run` 和三个低层扩展资源参数创建临时 Workspace：

```text
canary.hami.ihome.cat/gpu=1
canary.hami.ihome.cat/gpumem=2048
canary.hami.ihome.cat/gpucores=20
```

Workspace 内的 `nvidia-smi` 显示 2048 MiB，`cuInit(0)` 成功。由此可以确认 rc 的 Pod 生成、HAMi webhook 转换、DRA allocation、kubelet prepare 和 CDI/HAMi-core 注入能够连续工作。

## 其他观察

- HAMi Helm release 卸载后，driver 的 ResourceSlice 没有随 DaemonSet 自动删除。本次在重装前显式删除了 stale ResourceSlice。若后续频繁切换 driver，应把这项检查加入 runbook。
- `oci-ml` CUDA 和 PyTorch 镜像的节点侧冷拉取需要数分钟。等待期间 DRA claim 已 prepare，Pod 状态保持 `ContainerCreating`；这类延迟需要和调度或 driver 故障区分。
- `rcctl run --rm` 的临时 Workspace 在命令退出后没有立即删除。显式删除 Workspace 后，Pod、PVC、ResourceClaim 和 HAMi CDI claim 均正常回收。这一行为与 HAMi 的 claim 清理不是同一个问题。
- 当前 `rcctl --gpu-vram` 接受 Kubernetes quantity，例如 `16Gi`；HAMi-DRA webhook 把扩展资源的裸数值解释为 MiB。迁移标准资源名之前，需要统一 rc 的显存单位语义。

## 后续事项

1. 根据「自签证书模式下的 caBundle 问题」整理最小复现并提交 HAMi-DRA Issue。
2. 修正或收紧 rc 的 `--gpu-vram` 单位语义。
3. 调查 `rcctl run --rm` 延迟删除临时 Workspace 的原因。
4. 决定是否迁移 `liet-gpu-1`；迁移完成后可把 canary 资源名切回 `nvidia.com/gpu`、`nvidia.com/gpumem` 和 `nvidia.com/gpucores`。
