# Windows GUI workloads in containers and Kubernetes: community findings

Research date: 2026-09-06.

Follow-up: the [live Electron experiment](windows-electron-experiment.md)
records actual container rendering, screenshots, and Windows image builds.
The statements below about untested mechanisms describe the research phase.

## Conclusion

The blanket claim that Windows containers cannot execute GUI applications is too strong. There are concrete projects that start native Win32/WPF applications in ordinary Windows containers and automate their controls. A recent project also publishes a container desktop launcher and screenshot implementations. These are meaningful leads for an Electron experiment, but do not demonstrate that Electron and AUV work unchanged in an ordinary Windows container. The distinction is between starting a GUI process, obtaining an image of its UI, accepting desktop input, and reproducing a complete Windows desktop experience. The evidence for each differs.

| Approach | Evidence | Relevance to Electron |
|---|---|---|
| Container-local `WinSta0\\Default` plus native WebDriver instrumentation | Working source and author's container report in `xlazom00/winapi-webdriver` | Strong experiment candidate; no Electron demonstration |
| WPF UI Automation patterns inside a Windows container | PDQ example project and first-party test report | Proves GUI process/control automation can work; screenshot and physical input problems remain |
| Windows container display-resolution service | Microsoft executable artifacts, instructions, and user success report | Removes one real obstacle; does not supply a desktop compositor or prove screenshot fidelity |
| Historical RDP-in-container registry workaround | First-party demonstration, later explicitly reported broken | Historical proof of possibility, unsuitable as a current recipe |
| `dockur/windows` | QEMU implementation and Kubernetes manifest | Complete Windows VM desktop, normally on a Linux/KVM node |
| Wine with VNC | Linux/Wine Dockerfiles | Windows compatibility testing, not native Windows OS validation |

This investigation changed no cluster resources and ran no Windows GUI executables. Source inspection and author reports are separated below from results we would still need to reproduce.

## 1. Recent native container experiment: xlazom00/winapi-webdriver

Inspected revision: [`f0dbb3cd0f568775709bac63de96601977488371`](https://github.com/xlazom00/winapi-webdriver/commit/f0dbb3cd0f568775709bac63de96601977488371), dated 2026-08-16. The author linked this repository from the Windows Containers GUI issue and identifies the published demo as a pure WinAPI C++ app, with a separate VCL implementation used for their own application. This is a specific framework experiment, not a universal desktop automation implementation. [Author's project link and scope](https://github.com/microsoft/Windows-Containers/issues/611#issuecomment-5218771308)

The most useful mechanism is [`docker/start-on-desktop.ps1`](https://github.com/xlazom00/winapi-webdriver/blob/f0dbb3cd0f568775709bac63de96601977488371/docker/start-on-desktop.ps1). It runs **inside the container** and calls `CreateProcessW` with `STARTUPINFO.lpDesktop = "WinSta0\\Default"`. The author's explanation is that normal `docker exec` creates processes on a private `Service-0x0-...$` window station; switching the creation desktop fixes window visibility and dropdown behavior. This code does not acquire a host user token and does not explicitly select a different host session. It is a container-local workaround, distinct from launching onto the host's logged-in desktop.

The launcher returns a PID and closes its process handles without waiting. It does not implement application supervision, log forwarding, graceful shutdown, or crash reporting. A Kubernetes entrypoint adapting this technique should retain a supervisor that observes the child and returns its exit status; the demonstration script alone is not a complete workload lifecycle. [Launcher implementation](https://github.com/xlazom00/winapi-webdriver/blob/f0dbb3cd0f568775709bac63de96601977488371/docker/start-on-desktop.ps1)

The published [`Dockerfile`](https://github.com/xlazom00/winapi-webdriver/blob/f0dbb3cd0f568775709bac63de96601977488371/docker/Dockerfile) uses `mcr.microsoft.com/dotnet/framework/runtime:4.8.1-windowsservercore-ltsc2025` and installs/registers fonts. Its comments describe missing glyphs and bitmap font substitutions in container screenshots. This is an actual Windows base image, not Wine or a QEMU wrapper.

The repository contains two materially different screenshot paths:

- `TestApp4` captures the window into a bitmap with `PrintWindow(..., PW_RENDERFULLCONTENT)`, falls back to `BitBlt`, crops frame margins, and encodes PNG using WIC. This is capture code, with a WebDriver screenshot endpoint and browser inspector. It does not prove the resulting pixels are nonblank in every container or framework. [Capture implementation](https://github.com/xlazom00/winapi-webdriver/blob/f0dbb3cd0f568775709bac63de96601977488371/TestApp4/webdriver/WindowScreenshotter.cpp), [endpoint documentation](https://github.com/xlazom00/winapi-webdriver/blob/f0dbb3cd0f568775709bac63de96601977488371/TestApp4/webdriver/README.md)
- `TestApp3` asks controls to paint via `WM_PRINT` without `PRF_CHECKVISIBLE`, walks child controls, and composites an image. It **manually draws** single-line edit controls, window captions, menu bars, and placeholders for controls that fail to paint. Therefore its output cannot automatically be treated as a pixel-faithful screenshot of a real desktop. [Composite renderer, especially `REditFallback`, `RDrawPlaceholder`, and `RDrawNonClient`](https://github.com/xlazom00/winapi-webdriver/blob/f0dbb3cd0f568775709bac63de96601977488371/TestApp3/webdriver/WdRender.cpp)

Control interaction uses WinAPI messages such as `BM_CLICK`, `WM_SETTEXT`, and direct keyboard/mouse messages. It is coupled to native HWND/control semantics. Inferring that it also controls Chromium's rendered DOM would require evidence that the project does not provide. Electron should instead be evaluated with its own renderer automation/capture alongside an actual native-window screenshot check. [Control implementation](https://github.com/xlazom00/winapi-webdriver/blob/f0dbb3cd0f568775709bac63de96601977488371/TestApp4/webdriver/WinApiInspector.cpp)

**Evidence grade:** high confidence in the published mechanisms; moderate confidence in the reported WinAPI/VCL container behavior; unverified for Electron, AUV, the cluster's Windows build, and complete native window fidelity. There is enough evidence to justify testing this before declaring ordinary Windows container execution impossible.

## 2. Windows container display resolution: a real Microsoft workaround

Windows Containers [issue #527](https://github.com/microsoft/Windows-Containers/issues/527) initially received an incorrect environment-variable suggestion and a statement that changing resolution was unsupported. Later replies materially changed the answer:

1. Microsoft supplied `setres.exe`, run as `NT AUTHORITY\\SYSTEM`, with an example based on `windows/servercore:ltsc2025`. [Executable instructions](https://github.com/microsoft/Windows-Containers/issues/527#issuecomment-3116870180)
2. The reporter confirmed that they could set the resolution. [Success report](https://github.com/microsoft/Windows-Containers/issues/527#issuecomment-3142364000)
3. Microsoft then posted a service-based version using `server.exe install`, the `DisplayResolution` service, and `client.exe 1920 1080`. Its Dockerfile and three binaries are published on the repository's `resolution` branch. [Service instructions](https://github.com/microsoft/Windows-Containers/issues/527#issuecomment-3245875986), [published artifacts](https://github.com/microsoft/Windows-Containers/tree/resolution)

A later EKS user reported a window-station-switch failure on Server 2022, then said that running a SYSTEM HostProcess pod allowed setting resolution, explicitly noting that its working directory was on the host. That follow-up must not be mistaken for proof that the same command succeeded inside a normally isolated pod. [Failure](https://github.com/microsoft/Windows-Containers/issues/527#issuecomment-3771405900), [HostProcess result](https://github.com/microsoft/Windows-Containers/issues/527#issuecomment-3773767738)

**Evidence grade:** confirmed artifact plus user report for resolution changes; implementation internals were not available in the inspected branch's top-level artifacts. Resolution success is not proof of AUV display enumeration, physical input, or Electron rendering.

## 3. WPF applications running inside real Windows containers

PDQ's [`JasonNoonan/PDQBlogTestApp`](https://github.com/JasonNoonan/PDQBlogTestApp) includes a Windows `1809` Dockerfile, test executable, and PowerShell UI automation script. It starts the app, locates its main window using `Get-UiaWindow -Win32`, and invokes UI Automation control patterns. [Dockerfile](https://github.com/JasonNoonan/PDQBlogTestApp/blob/master/Dockerfile), [test script](https://github.com/JasonNoonan/PDQBlogTestApp/blob/master/Test-App.ps1)

The author's first-party report says this worked on Windows 10 1809 and Server 2019 hosts, with exceptions for menu behavior. Physical mouse/keyboard methods did not produce actions, and screenshot attempts produced black or transparent images. Invoke/Set/Expand control operations supplied the useful alternative. This establishes partial GUI automation, not a complete capturable desktop. [PDQ's experiment and limitations](https://www.pdq.com/blog/ui-testing-a-wpf-app-in-windows-containers/)

The Windows Containers team similarly distinguished availability of UI APIs in the larger Windows image from WinAppDriver's dependency on an active desktop session, and users reported behavior differences between 1809 and 1909. These version differences are a reason to test the exact image/runtime combination. [Microsoft explanation](https://github.com/microsoft/Windows-Containers/issues/47#issuecomment-759825786), [reported regression](https://github.com/microsoft/Windows-Containers/issues/47#issuecomment-760572095)

## 4. Historical RDP listener trick

Rafael Rivera demonstrated RDP into `microsoft/windowsservercore:1709_KB4074588` by setting `HKLM\\System\\CurrentControlSet\\Control\\Terminal Server\\TemporaryALiC=1` before Terminal Services started. His article contains a Dockerfile and screenshot. A January 2019 correction explicitly says that Microsoft broke the behavior after that image. The article also warns of interference with the host listener. This is a genuine historical workaround, but no current supported reproduction was found. [Original experiment and correction](https://withinrafael.com/2018/03/09/using-remote-desktop-services-in-containers/)

## 5. Complete Windows desktop through a container-managed VM

[`dockur/windows`](https://github.com/dockur/windows) actually starts QEMU. Its Kubernetes manifest mounts `/dev/kvm`, provides persistent storage, and publishes the web console and RDP ports. This is a useful way for Kubernetes to manage a native Windows desktop VM, with the Windows GUI inside the guest. It does not add GUI support to a standard Windows container on the existing Windows kubelet. [QEMU startup](https://github.com/dockur/windows/blob/ef4094039a90ecdc8f0402e63aff881ce79bf30f/src/entry.sh), [Kubernetes manifest](https://github.com/dockur/windows/blob/ef4094039a90ecdc8f0402e63aff881ce79bf30f/kubernetes.yml)

The inspected README supports a Linux host with KVM, or Windows 11 Docker/Podman with nested virtualization. It links WinBoat, WinPodX, and WinApps as desktop frontends using this backend. On the present cluster this route naturally fits a Linux/KVM node; using the Windows machine would introduce the separate Linux/nested-virtualization environment described by the project. [Requirements and desktop frontends](https://github.com/dockur/windows/blob/ef4094039a90ecdc8f0402e63aff881ce79bf30f/readme.md)

## 6. Wine is a different kind of result

[`huan/docker-windows`](https://github.com/huan/docker-windows) advertises Windows GUI apps in Docker, but explicitly uses Wine in a Linux image, with TigerVNC and a browser viewer. This may be useful for compatibility tests, but cannot establish native Windows behavior for shell integration, dialogs, rendering, or OS automation. [README](https://github.com/huan/docker-windows/blob/main/README.md), [Dockerfile](https://github.com/huan/docker-windows/blob/main/Dockerfile)

## 7. AUV already supplies Windows capture and input

Inspected AUV revision: [`a7b2fc8adca3acd4f3ff61867995a49f4c16a9bd`](https://github.com/moeru-ai/auv/commit/a7b2fc8adca3acd4f3ff61867995a49f4c16a9bd).

The Windows driver has display capture through `xcap`, window capture through `PrintWindow(PW_RENDERFULLCONTENT)`, UI Automation inspection, and foreground input through `SendInput`. These are implemented capabilities, not merely a roadmap. The window-capture API overlaps the recent WinAPI container experiment, which makes AUV a useful candidate for a reproduction. API overlap does not establish successful Electron pixels. [Driver README](https://github.com/moeru-ai/auv/blob/a7b2fc8adca3acd4f3ff61867995a49f4c16a9bd/crates/auv-driver-windows/README.md), [capture implementation](https://github.com/moeru-ai/auv/blob/a7b2fc8adca3acd4f3ff61867995a49f4c16a9bd/crates/auv-driver-windows/src/capture.rs)

Two existing assumptions matter:

- The permission probe considers session ID zero non-interactive, and readiness treats a missing interactive session as a hard blocker. A container-local window station may therefore require a more precise readiness check; simply placing the current AUV binary in a pod is not proven sufficient. This proposed adaptation must test actual window-station, capture, and input capabilities rather than blindly bypassing the check. [Permission probe](https://github.com/moeru-ai/auv/blob/a7b2fc8adca3acd4f3ff61867995a49f4c16a9bd/crates/auv-driver-windows/src/permission.rs), [readiness contract](https://github.com/moeru-ai/auv/blob/a7b2fc8adca3acd4f3ff61867995a49f4c16a9bd/crates/auv-driver-windows/README.md)
- Its documentation says background message input is unreliable for Chromium/Electron. A successful screenshot alone would not prove mouse/keyboard automation. Test foreground input in the target desktop or use Electron's renderer automation for DOM interaction. [Windows input caveats](https://github.com/moeru-ai/auv/blob/a7b2fc8adca3acd4f3ff61867995a49f4c16a9bd/crates/auv-driver-windows/README.md)

AUV also documents implemented Windows daemon/runner IPC through owner-scoped named pipes and a paired remote-device route. Its recorded tests prove transport and `display.list` routing, explicitly not live application interaction. This can carry automation requests between a Kubernetes controller and a desktop-side AUV process. [Windows IPC handoff](https://github.com/moeru-ai/auv/blob/a7b2fc8adca3acd4f3ff61867995a49f4c16a9bd/docs/ai/references/session-api/2026-08-16-windows-local-runner-ipc-handoff.md)

## 8. HostProcess and GUI process supervision

Kubernetes can manage a host-side controller using `hostProcess: true`, `hostNetwork: true`, and an appropriate Windows account. The session-launch bootstrap may require LocalSystem. This gives a path to use the existing Windows node even if the ordinary-container experiment fails. It does not automatically select the logged-in desktop. [HostProcess requirements](https://kubernetes.io/docs/tasks/configure-pod-container/create-hostprocess-pod/)

There are concrete community implementations for the session transition:

- [`murrayju/CreateProcessAsUser`](https://github.com/murrayju/CreateProcessAsUser/blob/dc9e18a460ea38ba82e82cf4c80222fd7cf64813/ProcessExtensions/ProcessExtensions.cs) obtains a user-session token, builds the user environment, sets `winsta0\default`, and calls `CreateProcessAsUser`.
- [`KelvinTegelaar/RunAsUser`](https://github.com/KelvinTegelaar/RunAsUser) exposes waiting, output capture, non-elevated launch, and a job-breakaway option for SYSTEM-originated user-session scripts.
- [`LizardByte/Sunshine`](https://github.com/LizardByte/Sunshine/blob/d7762276d5b7f497587d331e8b58d5d8aec00133/src/platform/windows/misc.cpp) uses `WTSQueryUserToken` and `CreateProcessAsUserW`, including job-breakaway handling, to launch applications under the console user's profile.
- [PsExec](https://learn.microsoft.com/en-us/sysinternals/downloads/psexec) provides `-i <session>` for an interactive launch. [Task Scheduler interactive-token tasks](https://learn.microsoft.com/en-us/windows/win32/taskschd/security-contexts-for-running-tasks) are another existing broker mechanism.

Process management requires special care across this transition. The inspected hcsshim source creates HostProcess containers around a Windows Job Object, enables termination when its last handle closes, and terminates all job members on container termination. Microsoft documents that processes in a job must share its session. Consequently, a cross-session GUI launch must not assume it inherits the HostProcess container's job, cleanup, or resource accounting. A breakaway flag is not universally available: parent job policy controls whether it succeeds. [hcsshim implementation](https://github.com/microsoft/hcsshim/blob/4712998fa57874aa7c28963702ba0c2213e3ebbf/internal/jobcontainers/jobcontainer.go), [job assignment and session restriction](https://learn.microsoft.com/en-us/windows/win32/api/jobapi2/nf-jobapi2-assignprocesstojobobject)

Proposed integration, not implemented or tested here:

```text
rc / Kubernetes
  -> HostProcess controller
     -> authenticated IPC to a supervisor in the user desktop session
        -> Node / pnpm / Electron process tree
        -> AUV capture and input worker
```

Use a task or service broker to bootstrap the session-side supervisor, or start it at user logon. The supervisor owns a separate Windows Job Object for the application process tree, captures logs and exit status, and handles stop requests. A heartbeat/lease timeout should terminate stale workload jobs when the controller disappears; a normal termination hook alone cannot cover forced pod deletion. Job Objects provide process-group termination and resource limits, but Electron's own child-process/job behavior must be validated. These are design proposals derived from the documented job APIs, not existing rc features. [Windows Job Objects](https://learn.microsoft.com/en-us/windows/win32/procthread/job-objects)

For the ordinary-container route, keep the launcher and GUI descendants in the container and have the supervisor retain process handles, forward logs, and wait. It avoids the intended cross-session handoff, but stop/restart behavior still needs runtime verification.

## 9. Concrete next experiment for this cluster

The earlier live check found `luoling-win11` Ready at `10.0.0.161`, build `10.0.26100.9168`, containerd `2.2.5`, with 8 CPUs and approximately 16 GiB RAM. Its taint is `os=windows:NoSchedule`. These are observations from this session, not project compatibility claims. No host or cluster changes were made during this research.

Run two separately evaluated probes:

1. An explicitly Windows-scheduled disposable pod, with a compatible image, fonts, and a supervised `WinSta0\Default` launcher. Start a small real Windows Electron app and AUV on the same desktop. Check native-window pixels, renderer pixels, input, and process cleanup after pod stop. Only then try the an Electron development workload.
2. If container rendering/input is insufficient, use a HostProcess controller and an interactive-session supervisor on the host. Validate AUV capture and Electron input there, then test stop, crash, restart, and controller-disconnection cleanup.

The second route depends on an available desktop session/display. The first route specifically investigates whether the community workaround avoids that host-session requirement for this workload. Neither route has been validated end-to-end on this node. The existing rc guide still describes Linux-only Workspace bootstrap and runner images; Windows orchestration is integration work beyond merely selecting a Windows node. [Current rc support boundary](../guides/experimental-windows-11-worker.md#rc-support-boundary)

## Search method and limits

Used GitHub CLI repository search, issue search, code search, and issue-comment API reads; then shallow-cloned the promising projects and inspected their source. Searches included `windows-containers`, `windows-in-docker`, `windows-container-desktop`, `PrintWindow container`, `VNC` in Windows Dockerfiles, and GUI issues in `microsoft/Windows-Containers`. GitHub code search hit a per-user HTTP 403 rate limit during the investigation; repository/issue searches and direct reads remained usable. Public web search located the first-party PDQ and Rafael Rivera reports. No conclusion relies on search snippets alone.

No current generic ordinary-Windows-container RDP/VNC desktop project was verified. This is a search result, not proof of impossibility. The most promising newly found ordinary-container path is the `WinSta0\\Default` launcher plus framework-aware capture, and its Electron suitability remains a concrete runtime experiment.
