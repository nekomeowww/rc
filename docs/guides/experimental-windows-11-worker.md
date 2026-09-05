<!-- cspell:ignore containerd cgroups kubeadm kubeconfig kubelet kubeproxy -->
<!-- cspell:ignore kubectl Flannel flanneld Geneve healthz hnsendpoint -->
<!-- cspell:ignore hostgw iface ipam ipmasq kernelspace ltsc masq nanoserver -->
<!-- cspell:ignore npipe nodename nodeport NSSM powershell proxier resolv -->
<!-- cspell:ignore systemverification VXLAN winkernel WinSW datapath -->
<!-- cspell:ignore daemonset -->

# Experimental Windows 11 Kubernetes Worker

> [!WARNING]
> This configuration is for experimental and development use only. Upstream
> Kubernetes supports Windows worker nodes on designated Windows Server
> releases, not Windows 11. Do not use this design for production workloads,
> security boundaries, or data that cannot be recreated. Test every Kubernetes,
> container runtime, CNI, Windows, and network-driver update before applying it
> to a shared cluster.

This guide describes how to attach a native Windows 11 worker to a mixed-OS
Kubernetes cluster. The proved design uses:

- containerd, kubelet, and winkernel kube-proxy as Windows services;
- an HNS L2Bridge network created by Flannel with the `host-gw` backend;
- Flannel as the common cross-OS CNI and route owner;
- Cilium chained behind Flannel on Linux only; and
- a taint and label that keep Linux workloads and Cilium off Windows.

The design carries pod packets as ordinary routed IP packets. It does not run
Cilium on Windows and does not use VXLAN or Geneve between nodes.

The setup was validated with Kubernetes 1.36.3, Flannel 0.28.8, Cilium 1.20.0,
containerd 2.2.5, and Windows 11 build 26100. Those versions describe the
experiment, not a compatibility promise.

## rc support boundary

Joining the node does not make current rc Workspaces Windows-compatible. The
published rc runner and Workspace bootstrap currently rely on Linux paths and
programs such as `/bin/sh` and `/workspace`.

Keep the Windows node tainted until rc has a Windows runner image and Windows
implementations for Workspace bootstrap, filesystem layout, process control,
terminal handling, and lifecycle commands. The node is still useful for
developing and testing that support and for running explicitly Windows-aware
Kubernetes workloads.

## Network model

Assume the following example ranges and replace them for your cluster:

| Purpose | Example |
| --- | --- |
| Node underlay | `192.0.2.0/24` |
| Pod CIDR | `10.244.0.0/16` |
| Per-node pod CIDR | one `/24` per node |
| Service CIDR | `10.96.0.0/12` |
| Cluster DNS | `10.96.0.10` |

Every host-gw address must be reachable from every other node without NAT.
They can share one layer-2 network or use infrastructure routes. Wi-Fi also
requires client-to-client traffic to be allowed by the access point.

```text
Linux pod
   |
   v
Flannel CNI -> Cilium chained endpoint -> Linux routing table
                                           |
                                ordinary routed pod packet
                                           |
                                           v
                              Windows underlay address
                                           |
                              Flannel + HNS L2Bridge
                                           |
                                       Windows pod
```

On a multi-homed Linux node, the Flannel public address and Cilium direct
routing device must use the same reachable network. An asymmetric return path
can make direct pod traffic work while remote NodePort traffic fails.

## Prerequisites

Before touching the Windows host:

1. Back up etcd and the current CNI configuration.
2. Confirm that the cluster allocates a unique pod CIDR to every node.
3. Confirm direct reachability among all intended host-gw addresses.
4. Pin Kubernetes node components to a version allowed by the cluster's
   [version-skew policy](https://kubernetes.io/releases/version-skew-policy/).
5. Choose Windows container images compatible with the host build. See
   [Windows container version compatibility](https://learn.microsoft.com/virtualization/windowscontainers/deploy-containers/version-compatibility).
6. Make every Linux-only DaemonSet select `kubernetes.io/os: linux`, especially
   DaemonSets with a catch-all `Exists` toleration.

The upstream references assume Windows Server. Read them for component layout
and operational context, but retain the warning at the top of this guide:

- [Kubernetes Windows overview](https://kubernetes.io/docs/concepts/windows/intro/)
- [SIG Windows tools node guide](https://github.com/kubernetes-sigs/sig-windows-tools/blob/master/guides/guide-for-adding-windows-node.md)
- [Flannel requirements and backends](https://github.com/flannel-io/flannel/blob/master/Documentation/running.md)
- [Cilium CNI chaining](https://docs.cilium.io/en/stable/installation/cni-chaining/)
- [Cilium native routing](https://docs.cilium.io/en/stable/network/concepts/routing/)

## Prepare the Linux datapath

If the cluster already uses Cilium as its primary CNI, it must not remain the
cross-OS overlay owner. Install Flannel host-gw on Linux, then configure Cilium
as a chained Linux datapath.

A representative Cilium values fragment is:

```yaml
cni:
  chainingMode: flannel
  exclusive: false

routingMode: native
autoDirectNodeRoutes: false
ipv4NativeRoutingCIDR: 10.244.0.0/16

ipam:
  mode: delegated-plugin

enableIPv4Masquerade: false
kubeProxyReplacement: true

endpointHealthChecking:
  enabled: false

extraArgs:
  - --local-router-ipv4=169.254.23.0

k8sServiceHost: <direct-api-address>
k8sServicePort: 6443
```

The ownership boundary is important:

- Flannel creates pod interfaces, allocates addresses, owns host-gw routes, and
  performs IPv4 masquerading.
- Cilium attaches to Linux pod interfaces, implements Linux Services and
  Linux network policy, and does not create a second overlay.
- Native Windows kube-proxy implements Services on Windows.
- Cilium policy does not enforce Windows endpoints. Linux policies can allow a
  trusted Windows pod CIDR with `ipBlock`, but cannot select Windows workload
  identities managed by Cilium.

Exclude Windows before admitting workloads. The kubelet configuration later
in this guide registers both properties from its first start:

```text
taint: os=windows:NoSchedule
label: cilium.io/no-schedule=true
```

Ensure the Cilium DaemonSet also has `kubernetes.io/os: linux`. On heterogeneous
or multi-homed Linux nodes, use per-node Cilium configuration to select the
same direct-routing interface that owns the Flannel public address.

Verify Linux before continuing:

```sh
kubectl -n kube-flannel get daemonset kube-flannel-ds
kubectl -n kube-system get daemonset cilium
kubectl -n kube-system exec daemonset/cilium -- cilium-dbg status --brief
kubectl get nodes -o wide
```

## Prepare Windows

Run preparation commands in an elevated PowerShell session. Enable the
Windows Containers feature and reboot if Windows requests it:

```powershell
Enable-WindowsOptionalFeature -Online -FeatureName Containers -All
```

Install these files from their official releases, pinning compatible versions:

```text
C:\Program Files\containerd\containerd.exe
C:\k\kubeadm.exe
C:\k\kubelet.exe
C:\k\flannel\kube-proxy.exe
C:\k\flannel\flanneld.exe
C:\opt\cni\bin\flannel.exe
C:\opt\cni\bin\win-bridge.exe
C:\opt\cni\bin\host-local.exe
```

Use the Kubernetes SIG Windows preparation scripts as a reference rather than
downloading an unpinned script directly into an administrator shell. Configure
containerd's CNI directories as:

```toml
[plugins."io.containerd.grpc.v1.cri".cni]
  bin_dir = "C:\\opt\\cni\\bin"
  conf_dir = "C:\\etc\\cni\\net.d"
```

Install containerd as an automatic LocalSystem service and verify its CRI
named pipe before proceeding:

```powershell
Get-Service containerd
Test-Path '\\.\pipe\containerd-containerd'
```

## Configure Flannel and CNI

Create `C:\k\flannel\net-conf.json`:

```json
{
  "Network": "10.244.0.0/16",
  "EnableIPv4": true,
  "EnableIPv6": false,
  "Backend": {
    "Type": "host-gw",
    "Name": "cbr0",
    "DNSServerList": "10.96.0.10"
  }
}
```

Create `C:\etc\cni\net.d\10-flannel.conf`:

```json
{
  "name": "cbr0",
  "cniVersion": "0.3.1",
  "type": "flannel",
  "subnetFile": "C:/run/flannel/subnet.env",
  "dataDir": "C:/var/lib/cni/flannel",
  "capabilities": {
    "portMappings": true,
    "dns": true
  },
  "delegate": {
    "apiVersion": 2,
    "type": "win-bridge",
    "loopbackDSR": true,
    "policies": [
      {
        "name": "EndpointPolicy",
        "value": {
          "Type": "OutBoundNAT",
          "Settings": {
            "Exceptions": [
              "10.244.0.0/16",
              "10.96.0.0/12",
              "<node-underlay-cidr>"
            ]
          }
        }
      }
    ]
  }
}
```

Create a dedicated, least-privilege kubeconfig for the host Flannel and
kube-proxy services. Do not copy `admin.conf` to the Windows node. The identity
needs Flannel's node/subnet-manager permissions and the built-in
`system:node-proxier` role. Store the kubeconfig outside user profiles and
restrict its ACL:

```powershell
$acl = Get-Acl C:\etc\kubernetes\network-agent.conf
$acl.SetAccessRuleProtection($true, $false)
$acl.Access | ForEach-Object { [void]$acl.RemoveAccessRule($_) }

$inheritance = [Security.AccessControl.InheritanceFlags]::None
$propagation = [Security.AccessControl.PropagationFlags]::None
$allow = [Security.AccessControl.AccessControlType]::Allow

$acl.AddAccessRule([Security.AccessControl.FileSystemAccessRule]::new(
  'SYSTEM', 'FullControl', $inheritance, $propagation, $allow))
$acl.AddAccessRule([Security.AccessControl.FileSystemAccessRule]::new(
  'BUILTIN\Administrators', 'FullControl', $inheritance, $propagation, $allow))

Set-Acl C:\etc\kubernetes\network-agent.conf $acl
```

Run Flannel with the Windows node name and its routable underlay address:

```powershell
$env:NODE_NAME = '<windows-node-name>'

C:\k\flannel\flanneld.exe `
  --ip-masq `
  --kube-subnet-mgr `
  --kubeconfig-file=C:\etc\kubernetes\network-agent.conf `
  --iface=<windows-underlay-address> `
  --public-ip=<windows-underlay-address> `
  --net-config-path=C:\k\flannel\net-conf.json `
  --subnet-file=C:\run\flannel\subnet.env `
  --healthz-port=8081 `
  --v=4
```

Install this command behind a service wrapper such as WinSW or NSSM. Configure
the service as LocalSystem, Automatic, and dependent on containerd. Do the same
for kubelet and kube-proxy; do not rely on an interactive PowerShell window.

## Configure kubelet and join

The persistent kubelet command needs the flags generated by kubeadm plus these
Windows-specific and scheduling settings:

```powershell
C:\k\kubelet.exe <kubeadm-flags> `
  --config=C:\var\lib\kubelet\config.yaml `
  --bootstrap-kubeconfig=C:\etc\kubernetes\bootstrap-kubelet.conf `
  --kubeconfig=C:\etc\kubernetes\kubelet.conf `
  --cert-dir=C:\var\lib\kubelet\pki `
  --hostname-override=<windows-node-name> `
  --node-ip=<windows-underlay-address> `
  --register-with-taints=os=windows:NoSchedule `
  --node-labels=cilium.io/no-schedule=true `
  --cgroups-per-qos=false `
  --enforce-node-allocatable="" `
  --resolv-conf=""
```

Generate a short-lived join command on the control plane. Add the containerd
pipe and node name on Windows. Windows 11 fails kubeadm's supported-OS check,
so this experimental setup bypasses only `SystemVerification`:

```powershell
C:\k\kubeadm.exe join <api-address>:6443 `
  --token <short-lived-token> `
  --discovery-token-ca-cert-hash sha256:<ca-hash> `
  --cri-socket npipe:////./pipe/containerd-containerd `
  --node-name <windows-node-name> `
  --ignore-preflight-errors=SystemVerification
```

Do not bypass all preflight errors. Delete or allow the bootstrap token to
expire after the join succeeds.

Start services in this order:

1. containerd
2. kubelet
3. Flannel
4. winkernel kube-proxy

Flannel creates the HNS `cbr0` L2Bridge and root `cbr0_ep` endpoint. Run
kube-proxy only after that network exists:

```powershell
Import-Module HostNetworkingService
while (-not (Get-HnsNetwork | Where-Object Name -eq 'cbr0')) {
  Start-Sleep -Seconds 1
}

$env:KUBE_NETWORK = 'cbr0'

C:\k\flannel\kube-proxy.exe `
  --hostname-override=<windows-node-name> `
  --proxy-mode=kernelspace `
  --kubeconfig=C:\etc\kubernetes\network-agent.conf `
  --enable-dsr=true `
  --root-hnsendpoint-name=cbr0_ep `
  --feature-gates=WinDSR=true `
  --v=4
```

## Route control-plane endpoint addresses

The Kubernetes Service for the API can select endpoint addresses on a
management network that is not directly reachable from the Windows underlay.
In that case, DNS and ordinary Services can work while a Windows pod request to
`kubernetes.default.svc` times out.

Add one persistent `/32` host route for each API endpoint through a reachable
Linux underlay address:

```powershell
New-NetRoute `
  -DestinationPrefix '<api-endpoint>/32' `
  -InterfaceAlias '<windows-underlay-interface>' `
  -NextHop '<linux-underlay-next-hop>' `
  -RouteMetric 5
```

Do not pass `-PolicyStore PersistentStore`; that value is not accepted by
`New-NetRoute`. With no `PolicyStore` argument, Windows creates the route in
both active and persistent stores. Verify both the host and pod path:

```powershell
Get-NetRoute -AddressFamily IPv4 -PolicyStore PersistentStore
Test-NetConnection <api-endpoint> -Port 6443

kubectl exec <windows-test-pod> -- `
  curl.exe --max-time 10 -k https://kubernetes.default.svc/version
```

See the [`New-NetRoute` documentation](https://learn.microsoft.com/powershell/module/nettcpip/new-netroute)
for current parameter behavior.

## Verify the node

Check registration and exclusion:

```sh
kubectl get node <windows-node-name> -o wide
kubectl get node <windows-node-name> --show-labels
kubectl describe node <windows-node-name>
kubectl -n kube-system get pods -l k8s-app=cilium -o wide
```

Expected state:

- the Windows node is Ready;
- it owns one unique pod CIDR;
- `NetworkUnavailable` is false;
- its Flannel public-IP annotation contains the underlay address;
- it has `os=windows:NoSchedule` and `cilium.io/no-schedule=true`; and
- no Cilium pod runs on it.

On each Linux node, verify a host-gw route to the Windows pod CIDR:

```sh
ip route show <windows-pod-cidr>
```

On Windows, verify HNS and services:

```powershell
Get-HnsNetwork | Where-Object Name -eq 'cbr0'
Get-HnsEndpoint | Where-Object Name -eq 'cbr0_ep'
Get-Service containerd,kubelet,flanneld,kube-proxy-flannel
```

Use disposable Linux and Windows pods to test all of these paths before
admitting development workloads:

1. Linux pod to Windows pod IP.
2. Windows pod to Linux pod IP.
3. DNS and ClusterIP in both directions.
4. Windows pod to `kubernetes.default.svc`.
5. NodePort through every Linux and Windows underlay address.
6. Linux network-policy allow and deny behavior.
7. Restarts of Flannel, kube-proxy, and kubelet without recreating workloads.

Delete the test namespace after recording the result.

## DaemonSet scheduling audit

A catch-all toleration defeats the Windows taint. Audit cluster-wide
DaemonSets after joining the node:

```sh
kubectl get daemonsets -A -o wide
kubectl get pods -A --field-selector spec.nodeName=<windows-node-name> -o wide
```

Patch any Linux-only DaemonSet that lacks an OS selector:

```sh
kubectl -n <namespace> patch daemonset <name> --type merge --patch-file - <<'EOF'
spec:
  template:
    spec:
      nodeSelector:
        kubernetes.io/os: linux
EOF
```

Expect a rolling restart of that DaemonSet on its Linux nodes.

## Reset and retry

If a failed experiment leaves stale CNI or HNS state, drain and delete the
Windows Node object before resetting the host. Back up the relevant directories
and HNS inventory, then stop services:

```powershell
Stop-Service kube-proxy-flannel,kubelet,flanneld,containerd
C:\k\kubeadm.exe reset --force
```

After confirming that no workload uses the experimental network, remove only
the HNS network named `cbr0`. Never bulk-delete HNS networks because Hyper-V,
WSL, and the Default Switch may share the host:

```powershell
$network = Get-HnsNetwork | Where-Object Name -eq 'cbr0'
if ($network) {
  Get-HnsEndpoint |
    Where-Object VirtualNetwork -eq $network.Id |
    Remove-HnsEndpoint
  $network | Remove-HnsNetwork
}
```

Move the stale `C:\run\flannel` subnet file and `C:\var\lib\cni\networks\cbr0`
allocation state into a timestamped backup instead of deleting broad parent
directories. Start containerd, obtain a new short-lived join token, and repeat
the join.

## Known limitations

- Windows 11 is outside the upstream Kubernetes Windows-node support matrix.
- Cilium does not manage or enforce Windows endpoints in this topology.
- Cilium features that require ownership of the primary CNI or an encapsulation
  device may be unavailable in chained native-routing mode.
- Windows host-gw depends on stable underlay addressing and symmetric routes.
- Windows container host/image compatibility is stricter than Linux container
  compatibility.
- A successful service restart is not a substitute for a controlled reboot
  test before relying on the node for unattended development.
- Current rc Workspace runtime images and bootstrap behavior are Linux-only.
