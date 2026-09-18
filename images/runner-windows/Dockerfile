# escape=`

# Native Windows Workspace runtime and development tools.
# Assets are prepared on Windows; no credentials or application source belong here.
# Visual Studio's managed build tools require the .NET Framework base rather
# than bare Server Core. This remains a Windows Server Core LTSC 2025 image.
ARG WINDOWS_IMAGE=mcr.microsoft.com/dotnet/framework/runtime:4.8.1-windowsservercore-ltsc2025

FROM ${WINDOWS_IMAGE} AS runner

USER ContainerAdministrator

SHELL ["powershell.exe", "-NoLogo", "-NoProfile", "-Command", `
    "$ErrorActionPreference = 'Stop'; $ProgressPreference = 'SilentlyContinue';"]

COPY assets/node/ C:/rc/node/
COPY assets/git/ C:/rc/git/
COPY assets/fonts/ C:/rc/fonts/
COPY assets/python-amd64.exe C:/rc/python-amd64.exe
COPY assets/cmake-x64.msi C:/rc/cmake-x64.msi
COPY assets/vs_buildtools.exe C:/rc/vs_buildtools.exe
COPY assets/vs-layout/ C:/rc/vs-layout/
COPY assets/vc_redist.x64.exe C:/rc/vc_redist.x64.exe
COPY install-tools.ps1 C:/rc/

RUN powershell.exe -NoProfile -File C:\rc\install-tools.ps1

COPY entrypoint.ps1 register-fonts.ps1 verify-toolchain.ps1 C:/rc/
RUN & C:\rc\register-fonts.ps1 -Install

COPY assets/rc/ C:/rc/bin/

ENV APPDATA="C:\home\agent\AppData\Roaming" `
    HOME="C:\home\agent" `
    LOCALAPPDATA="C:\home\agent\AppData\Local" `
    NODE_GYP_FORCE_PYTHON="C:\Python3\python.exe" `
    npm_config_msvs_version="2022" `
    npm_config_python="C:\Python3\python.exe" `
    PATH="C:\rc\bin;C:\rc\node;C:\rc\git\cmd;C:\Python3;C:\Program Files\CMake\bin;C:\Windows\system32;C:\Windows;C:\Windows\System32\WindowsPowerShell\v1.0" `
    USERPROFILE="C:\home\agent" `
    XDG_CACHE_HOME="C:\tmp\cache"

# Verify that unprivileged Workspace processes can resolve the portable tools.
# entrypoint.ps1 loads VsDevCmd for runtime commands so cl.exe and MSBuild are
# also available without hard-coding the servicing-dependent MSVC directory.
USER ContainerUser
RUN & C:\rc\entrypoint.ps1 powershell.exe -NoProfile -File C:\rc\verify-toolchain.ps1; `
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

LABEL org.opencontainers.image.source="https://github.com/nekomeowww/rc" `
    org.opencontainers.image.title="Windows Workspace runner"

WORKDIR C:\workspace

ENTRYPOINT ["powershell.exe", "-NoLogo", "-NoProfile", `
    "-WorkspaceExecPolicy", "Bypass", "-File", "C:\\rc\\entrypoint.ps1"]
CMD ["rc-kube.exe", "serve"]

FROM runner AS codex

ARG CODEX_VERSION=0.154.0
ARG PNPM_VERSION=12.4.1
ARG BUN_VERSION=1.4.2

USER ContainerAdministrator

COPY assets/openai/ C:/rc/openai/

ENV PATH="C:\rc\openai\node_modules\.bin;${PATH}" `
    pnpm_config_store_dir="C:\tmp\cache\pnpm\store"

LABEL org.opencontainers.image.title="Windows Codex Workspace runtime" `
    org.opencontainers.image.version="${CODEX_VERSION}"

# Use the native Codex executable so process commands preserve their arguments.
# Batch shims require cmd.exe and cannot preserve arbitrary arguments safely.
RUN $codex = @(Get-ChildItem C:\rc\openai\node_modules\@openai -Recurse -Filter codex.exe); `
    if ($codex.Count -ne 1) { throw 'Expected one Windows Codex executable' }; `
    Copy-Item (Join-Path $codex[0].DirectoryName '*') C:\rc\bin -Recurse -Force; `
    $ripgrep = @(Get-ChildItem C:\rc\openai\node_modules\@openai -Recurse -Filter rg.exe); `
    if ($ripgrep.Count -eq 1) { Copy-Item $ripgrep[0].FullName C:\rc\bin\rg.exe }

RUN if ((& codex.exe --version) -ne ('codex-cli ' + $env:CODEX_VERSION)) { throw 'Unexpected Codex version' }; `
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }; `
    if ((& pnpm.cmd --version) -ne $env:PNPM_VERSION) { throw 'Unexpected pnpm version' }; `
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }; `
    if ((& bun.cmd --version) -ne $env:BUN_VERSION) { throw 'Unexpected Bun version' }; `
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

USER ContainerUser
