# Windows Workspace runtime images

This directory packages rc's native Windows runtime and a derived Codex image.
See the
[Windows Workspace guide](../../docs/guides/windows-workspaces.md) for integration.
See the [live experiment results](../../docs/research/windows-electron-experiment.md)
for what has actually passed on the Windows node.

The Dockerfile's `runner` target contains the .NET Framework runtime on Windows
Server Core LTSC 2025, rcctl, rc-kube, Node, Git, Python, CMake, Visual Studio
2022 Build Tools with the Desktop C++ toolchain and Windows 11 SDK, standard
Windows fonts, and the Visual C++ runtime. The `codex` target adds Codex CLI,
pnpm, and Bun. Both start through `entrypoint.ps1`, which loads the Visual Studio
developer environment and fonts into the container session before invoking the
supplied command. This makes `cl.exe` and MSBuild available to interactive
commands while preserving Visual Studio's registered-instance discovery for
node-gyp. Kubernetes owns the ordinary container's process lifetime. The
default command is `rc-kube.exe serve`; Windows Pod construction preserves the
image entrypoint.

Fonts are copied and registered system-wide during the administrator build step;
the runtime entrypoint only loads them into the unprivileged user's session.

## Assets

First run `make build-windows` in the repository to cross-compile the amd64
binaries, then copy `bin/windows-amd64` to the Windows build machine.

Stage assets on a Windows machine into the ignored `assets/` directory using
`prepare-assets.ps1`. Supply only the dedicated tool installation directories;
application source and user profiles do not belong in the build context.
`prepare-ci-assets.ps1` downloads the public Node, Python, CMake, Visual Studio
Build Tools, and Visual C++ inputs and uses the Git and font files installed on
a GitHub-hosted Windows runner. It is used by pull-request image validation and
tagged releases.

The tested inputs are:

- Node 26 Windows x64. The experiment used `26.8.1` from the [official distribution](https://nodejs.org/dist/v26.8.1/).
- Portable Git for Windows; the experiment used the node's `2.51.0.windows.2` installation.
- Python `3.13.15` x64 from the [official Python distribution](https://www.python.org/ftp/python/3.13.15/).
- CMake `4.3.5` x64 from the [official Kitware release](https://github.com/Kitware/CMake/releases/tag/v4.3.5).
- The [Visual Studio 2022 Build Tools bootstrapper](https://aka.ms/vs/17/release/vs_buildtools.exe), installing `Microsoft.VisualStudio.Workload.VCTools`, the x64/x86 MSVC tools, and Windows SDK 10.0.26100.
- The [Visual C++ x64 redistributable](https://aka.ms/vs/17/release/vc_redist.x64.exe).
- Arial, Arial Bold, Segoe UI, Segoe UI Bold, Tahoma, Tahoma Bold, and Microsoft Sans Serif from the Windows host's Fonts directory.
- A dedicated npm prefix containing `@openai/codex@0.147.0`, `pnpm@10.26.2`, and `bun@1.4.0`.

The caller selects the Node distribution, matching the Linux runner's Node 26
base-image family. The staging script runs `node.exe --version` to verify that
it executes and report the supplied version; it does not require a specific
patch release. The versions and hashes below record the earlier experiment.

The fixed build assets have these SHA-256 hashes:

```text
57693d8e93d1b04e7b7de46aca53ecd63e97564e73de36a68428d7ff08d83587  node-v26.8.1-win-x64.zip
edec09c4853aeae9ac36efb8c9f95b6b8e2fee65eee56d9767a8b7c69c574403  python-3.13.15-amd64.exe
dac5ddcd2d58699ebe1211173afabfe6f0ca24340e2e995f333cb3e00cff72d6  cmake-4.3.5-windows-x86_64.msi
```

The staging script verifies these three fixed-version assets before copying
them into the build context. Microsoft distributes the Visual Studio and VC
Redistributable bootstrappers through mutable official release URLs, so their
exact servicing build is not reproducible across dates. The script verifies
their Microsoft Authenticode signatures instead. They use unattended
installation and accept success with either exit code `0` or the
reboot-required code `3010`. The image removes the staged installers and
Package Cache after setup, and Build Tools uses `--nocache`.

Install the npm tools with the pinned Node on PATH, into a new dedicated prefix:

```powershell
npm.cmd install --prefix C:\rc-image-tools @openai/codex@0.147.0 pnpm@10.26.2 bun@1.4.0
```

Then stage the inputs (replace paths with the extracted installation paths):

```powershell
.\prepare-assets.ps1 `
  -RCBinaryDirectory C:\src\rc\bin\windows-amd64 `
  -NodeDirectory C:\downloads\node-v26.8.1-win-x64 `
  -GitDirectory C:\tools\git `
  -OpenaiDirectory C:\rc-image-tools `
  -PythonInstaller C:\downloads\python-3.13.15-amd64.exe `
  -CMakeInstaller C:\downloads\cmake-4.3.5-windows-x86_64.msi `
  -VSBuildToolsBootstrapper C:\downloads\vs_buildtools.exe `
  -VCRuntimeInstaller C:\downloads\vc_redist.x64.exe
```

For a BuildKit worker without build-step networking, first download a Visual
Studio layout on a networked Windows machine (this does not install tools on
that host):

```powershell
Start-Process C:\downloads\vs_buildtools.exe -Wait -ArgumentList @(
  '--layout', 'C:\downloads\vs-layout', '--lang', 'en-US', '--quiet', '--wait',
  '--add', 'Microsoft.VisualStudio.Workload.VCTools',
  '--add', 'Microsoft.VisualStudio.Component.VC.Tools.x86.x64',
  '--add', 'Microsoft.VisualStudio.Component.Windows11SDK.26100'
)
Invoke-WebRequest -UseBasicParsing `
  -Uri 'https://www.microsoft.com/pkiops/certs/Microsoft%20Windows%20Code%20Signing%20PCA%202024.crt' `
  -OutFile C:\downloads\vs-layout\Certificates\MicrosoftWindowsCodeSigningPCA2024.crt
```

Pass `-VSBuildToolsLayout C:\downloads\vs-layout` when staging into a fresh build
context. The image installer selects the layout with `--noWeb`; omit that
parameter for the normal online build. The layout increases transferred context
and image-layer size even though setup removes its files from the final
filesystem. See Microsoft's [offline installation instructions](https://learn.microsoft.com/en-us/visualstudio/install/create-an-offline-installation-of-visual-studio?view=vs-2022).
The installer imports pinned Microsoft roots and the signing intermediate into
the image only, keeping signature verification enabled. The separate intermediate
download follows Microsoft's [missing-certificate fix](https://learn.microsoft.com/en-us/troubleshoot/developer/visualstudio/installation/install-failure-2017-later-versions).

The native C++ workload and Windows SDK add several gigabytes to the unpacked
Windows image (typically about 7-10 GB, depending on the servicing release
selected by Microsoft's bootstrapper). Builds need at least 2 GB of memory and
a Windows container disk large enough for the base image, transient installer
payloads, and final Build Tools layers. Record the exact delta from
`docker image inspect` or `ctr images ls` in release validation because the
mutable Build Tools channel makes a single permanent size figure misleading.

The experiment's images contain locally supplied Windows fonts and were kept
in the node's local image store.

## Build with the Windows node's containerd

Use native Windows BuildKit. The tested version is
[BuildKit 0.33.0](https://github.com/moby/buildkit/releases/tag/v0.33.0), whose
Windows amd64 archive SHA-256 is
`5b4bc24d425f4dfdecf575d386ebf19db12b5f46fef9e95a776bcbaf3b4e486f`.

Run `buildkitd` as a native Windows service with its own root, named pipe, and
containerd namespace. Running it directly inside a HostProcess container hits
the bind-filter limitation documented in
[BuildKit #6590](https://github.com/moby/buildkit/issues/6590).
The [upstream Windows instructions](https://github.com/moby/buildkit/blob/v0.33.0/docs/windows.md)
describe native service registration. This experiment used
`npipe:////./pipe/rc-windows-buildkit` and namespace `rc-electron-experiment`.

From this directory on Windows:

```powershell
buildctl.exe --addr npipe:////./pipe/rc-windows-buildkit build `
  --frontend dockerfile.v0 --local context=. --local dockerfile=. `
  --opt platform=windows/amd64 --opt target=runner --opt image-resolve-mode=local `
  --output type=image,name=test.neko.local/nekomeowww/rc/runner-windows:integration-20260906,push=false

buildctl.exe --addr npipe:////./pipe/rc-windows-buildkit build `
  --frontend dockerfile.v0 --local context=. --local dockerfile=. `
  --opt platform=windows/amd64 --opt target=codex --opt image-resolve-mode=local `
  --output type=image,name=test.neko.local/nekomeowww/rc/openai-codex-cli-windows:integration-20260906,push=false
```

The context is this directory: every local `COPY` source is here, and the
`.dockerignore` allows only the Dockerfile, runtime scripts, and prepared assets.
`FROM runner` refers to the earlier build stage. No remote `ADD`, secret build
argument, or repository-wide source copy is used.

Export a built image and import it into Kubernetes' containerd namespace:

```powershell
$image = 'test.neko.local/nekomeowww/rc/runner-windows:integration-20260906'
ctr.exe -n rc-electron-experiment images export --platform windows/amd64 C:\runner-windows.tar $image
ctr.exe -n k8s.io images import --local --platform windows/amd64 `
  --label io.cri-containerd.image=managed C:\runner-windows.tar
```

Use `imagePullPolicy: Never` for these local experiment tags, select the Windows
node explicitly, and tolerate its Windows taint. Image building and loading
must happen on the Windows runtime; a Linux Docker daemon cannot run the result.

## Application dependencies and validation

### Local pnpm caches

The image sets `XDG_CACHE_HOME=C:\tmp\cache` and grants `ContainerUser` access
to the cache directories, without moving `HOME`, `APPDATA`, or `LOCALAPPDATA`.
Keep active source on the container writable layer as described in the Windows
guide. From the project's install root, use an independent virtual store per
checkout (replace `<checkout-id>`):

```powershell
pnpm.cmd install --store-dir=C:\tmp\cache\pnpm\store --virtual-store-dir=C:\tmp\cache\pnpm\virtual\<checkout-id>
```

Keep both path flags for later dependency changes and follow the repository's
lockfile and build-script policies. These caches are disposable and are not
included in persistent-home snapshots. After runtime replacement, restore source
and reinstall; if retained node_modules links target a missing virtual store,
stop affected processes, move only generated node_modules aside, reinstall, and
verify resolution. Do not default to `--force` or clear active caches.

This does not fix Windows mounted-directory rename limitations or reduce the
disk space needed to build the image's native toolchain.

### Runtime validation

The runner provides rc's native process runtime, development tools, fonts, and
system libraries. Applications install their own framework dependencies, such
as Electron, from their own repository. The image does not bundle an application,
Electron distribution, development watcher, or capture script.

Application integration fixtures belong in separate repositories. They should
exercise the actual Workspace Repository/Worktree mount path, including
installation, filesystem operations, development processes, and cleanup. The
current Windows Repository/Worktree mount limitation is described in the
[Windows guide](../../docs/guides/windows-workspaces.md).

The earlier experiment images bundled Electron and a temporary test application.
Their recorded digests and screenshots describe those historical builds. Rebuild
from the current Dockerfile for the generic runtime image; those results do not
validate Windows Repository/Worktree integration.

For a toolchain smoke check, run the image through its normal entrypoint as the
same user used by the Workspace and check `python.exe --version`,
`cmake.exe --version`, `cl.exe /?`, and `MSBuild.exe -version`. A native addon
fixture should then run its package's `node-gyp rebuild`; invoking the normal
entrypoint is important because it imports `VsDevCmd.bat` before starting the
command. Python and CMake are on the machine-level image PATH.
`NODE_GYP_FORCE_PYTHON=C:\Python3\python.exe`,
`npm_config_msvs_version=2022`, and the registered Build Tools instance allow
node-gyp to select the in-container Python and Visual Studio 2022 without a
CI-host installation.

The Dockerfile also runs `C:\rc\verify-toolchain.ps1` through the normal
entrypoint as `ContainerUser`. It compiles, links and runs a disposable C++
program using Windows SDK headers and the VC runtime. This gate catches missing
compiler libraries that version-only probes miss. Older experiment images with
only a cache layer do not provide this toolchain; rebuild the full `codex` target
before testing applications that invoke node-gyp.
