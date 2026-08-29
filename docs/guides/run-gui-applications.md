# Running GUI Applications in rc

This guide explains how to run GUI applications such as Electron in an rc
Workspace and capture their output with AUV.

The runtime uses headless Wayland. It does not require a physical display or
provide a complete GNOME or KDE desktop.

## Runtime layout

```text
rc Workspace
  ├─ rc-kube: manages application processes
  ├─ Sway: provides a virtual Wayland display
  ├─ PipeWire and WirePlumber: transport captured frames
  ├─ xdg-desktop-portal: exposes the screencast interface
  └─ AUV: captures the display and stores image artifacts
```

GUI applications launched by `rc-kube` inherit the same Wayland, D-Bus, and
PipeWire session. Applications should not start a separate desktop session.

## Prerequisites

Before starting, make sure that:

- rc is deployed and can create Workspaces and AgentProcesses;
- the cluster provides GPU drivers and the `nvidia.com/gpu` resource;
- a runner image containing Wayland, AUV, and `rc-kube` is available;
- nodes trust the registry certificate and the Workspace ServiceAccount has
  pull credentials when the image is private; and
- the application supports native Wayland or a Wayland backend provided by
  Electron, GTK, or Qt.

For software rendering, remove `nvidia.com/gpu` from the Workspace resources.
Performance and compatibility without a GPU must be evaluated separately.

## Step 1: Create a Workspace

Create the following resource and replace the image placeholder with the
Wayland runner image:

```yaml
apiVersion: workspaces.rc.ayaka.io/v1alpha1
kind: Workspace
metadata:
  name: gui-workspace
  namespace: default
spec:
  desiredState: Running
  image: "<wayland-runner-image>"
  storage:
    size: 20Gi
  resources:
    requests:
      cpu: 250m
      memory: 512Mi
      nvidia.com/gpu: "1"
    limits:
      cpu: "4"
      memory: 8Gi
      nvidia.com/gpu: "1"
```

Apply the resource and wait for the Workspace:

```sh
kubectl apply -f gui-workspace.yaml

kubectl wait \
  --for=condition=Ready \
  workspace/gui-workspace \
  --timeout=5m
```

rc does not yet include GUI health in the Workspace readiness probe. After the
Workspace becomes Ready, check the runtime explicitly:

```sh
kubectl exec gui-workspace -c rc-kube -- rc-wayland-health
```

A successful check confirms that the following components are available:

- the `rc-kube` socket;
- the Wayland display socket;
- PipeWire; and
- the Wayland screencast portal.

If the Workspace requested a GPU, verify the assigned device:

```sh
kubectl exec gui-workspace -c rc-kube -- \
  nvidia-smi --query-gpu=name,memory.total,driver_version --format=csv,noheader
```

## Step 2: Provide the application

Application dependencies can live in the persistent home directory or in a
derived image.

### User-level dependencies

Node.js packages, Electron, and other dependencies that do not require root can
be installed under `/home/agent`. This directory is stored on the Workspace
volume and survives runtime restarts.

For example, install Electron with an AgentProcess:

```sh
rcctl agent exec --workspace gui-workspace -- \
  bash -lc '
    set -eu
    mkdir -p /home/agent/apps/my-electron-app
    cd /home/agent/apps/my-electron-app
    if ! test -f package.json; then npm init -y; fi
    npm install --save-exact electron@44.0.0
  '
```

Application source code can also live in a Worktree mounted into the Workspace.

### System dependencies

Libraries that require `apt` should be added to a derived image. Regular
Workspaces run as a non-root user and cannot install system packages at
runtime.

```dockerfile
FROM <wayland-runner-image>

USER root
RUN apt-get update \
    && apt-get install -y --no-install-recommends <application-packages> \
    && rm -rf /var/lib/apt/lists/*

COPY --chown=1000:1000 . /opt/gui-app

USER 1000:1000
```

The derived image must retain the runner's `rc-kube` wrapper, s6 services, and
Wayland environment. Start the GUI application through an AgentProcess rather
than making it the Workspace container's main process.

## Step 3: Start the GUI application

A native Wayland application can be started directly:

```sh
rcctl agent run --detach --workspace gui-workspace -- \
  /opt/gui-app/bin/my-app
```

For Electron, use the rendering flags validated with this runtime:

```sh
rcctl agent run --detach --workspace gui-workspace -- \
  bash -lc '
    cd /home/agent/apps/my-electron-app
    exec ./node_modules/.bin/electron \
      --no-sandbox \
      --ozone-platform=wayland \
      --use-gl=angle \
      --use-angle=gl \
      --ignore-gpu-blocklist \
      --enable-gpu-rasterization \
      .
  '
```

`--detach` leaves the application running inside the Workspace. Inspect and
manage it with:

```sh
rcctl agent list --workspace gui-workspace
rcctl agent logs <process-id>
rcctl agent stop <process-id>
```

rc retains AgentProcess execution history. Reusing the same process identity
does not execute the application again; create a new AgentProcess when starting
an updated application.

## Step 4: Capture the display with AUV

List the virtual displays:

```sh
rcctl agent exec --workspace gui-workspace -- \
  auv invoke display.list --json
```

The default runtime exposes one 1280x720 virtual display. Capture it with:

```sh
rcctl agent exec --workspace gui-workspace -- \
  auv invoke display.capture \
    --store-root /home/agent/auv-runs \
    --json
```

A successful result includes:

- the display name and dimensions;
- the Wayland portal and PipeWire capture backend;
- the image format; and
- the artifact path inside the Workspace.

rcctl currently returns the artifact metadata and path but does not copy the
image to the caller. Download it using the path returned by AUV:

```sh
kubectl cp -c rc-kube \
  gui-workspace:/home/agent/auv-runs/<artifact-path>.png \
  ./capture.png
```

An agent integration may instead read the artifact and return it directly as
an image.

## Step 5: Stop the application or Workspace

Stop one application with:

```sh
rcctl agent stop <process-id>
```

When the GPU is no longer needed, stop the Workspace:

```sh
rcctl workspace stop gui-workspace
```

Stopping the Workspace releases its runtime Pod and GPU while retaining the
`/home/agent` volume. Starting it again creates a fresh Wayland session:

```sh
rcctl workspace start gui-workspace
```

## Troubleshooting

### The Workspace is Ready, but the application cannot connect to Wayland

Run the runtime health check:

```sh
kubectl exec gui-workspace -c rc-kube -- rc-wayland-health
```

If it fails, inspect the Workspace container logs and confirm that Sway,
PipeWire, WirePlumber, and the screencast portal are running.

### Electron shows only a background or black frame

Confirm that Electron uses native Wayland and the validated rendering backend:

```text
--ozone-platform=wayland
--use-gl=angle
--use-angle=gl
```

Do not treat a process appearing in `nvidia-smi` as sufficient evidence. Verify
that the captured image contains the application window.

### AUV creates a screencast session but receives no frames

Confirm that WirePlumber is running. PipeWire provides the media graph, while
WirePlumber connects the portal output to the AUV capture stream.

### A private image cannot be pulled

Check these independently:

1. The scheduled node trusts the registry certificate.
2. The Workspace ServiceAccount references valid image pull credentials.

The current rc implementation also omits `serviceAccountName` when ServiceAccount
token mounting is disabled. Keep token mounting enabled for private images
until these settings are decoupled.

### Electron reports a sandbox error

The restricted Workspace currently starts Electron with `--no-sandbox`. The
Kubernetes container security policy still applies, but Electron's Chromium
sandbox is disabled. Define an additional isolation strategy before loading
untrusted web content.
