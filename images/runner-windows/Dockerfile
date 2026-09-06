# escape=`

# Native Windows Workspace runtime and development tools.
# Assets are prepared on Windows; no credentials or application source belong here.
ARG WINDOWS_IMAGE=mcr.microsoft.com/windows/servercore:ltsc2025

FROM ${WINDOWS_IMAGE} AS runner

USER ContainerAdministrator

SHELL ["powershell.exe", "-NoLogo", "-NoProfile", "-Command", `
    "$ErrorActionPreference = 'Stop'; $ProgressPreference = 'SilentlyContinue';"]

COPY assets/node/ C:/rc/node/
COPY assets/git/ C:/rc/git/
COPY assets/fonts/ C:/rc/fonts/
COPY assets/vc_redist.x64.exe C:/rc/vc_redist.x64.exe
COPY entrypoint.ps1 register-fonts.ps1 C:/rc/

RUN $process = Start-Process C:\rc\vc_redist.x64.exe `
        -ArgumentList '/install', '/quiet', '/norestart' `
        -Wait `
        -PassThru; `
    if ($process.ExitCode -notin @(0, 3010)) { `
        throw "VC runtime installation failed: $($process.ExitCode)" `
    }; `
    Remove-Item C:\rc\vc_redist.x64.exe; `
    & C:\rc\register-fonts.ps1; `
    New-Item -ItemType Directory -Force C:\workspace | Out-Null

COPY assets/rc/ C:/rc/bin/

ENV APPDATA="C:\home\agent\AppData\Roaming" `
    HOME="C:\home\agent" `
    LOCALAPPDATA="C:\home\agent\AppData\Local" `
    PATH="C:\rc\bin;C:\rc\node;C:\rc\git\cmd;C:\Windows\system32;C:\Windows;C:\Windows\System32\WindowsPowerShell\v1.0" `
    USERPROFILE="C:\home\agent"

LABEL org.opencontainers.image.source="https://github.com/nekomeowww/rc" `
    org.opencontainers.image.title="Windows Workspace runner"

WORKDIR C:/workspace

ENTRYPOINT ["powershell.exe", "-NoLogo", "-NoProfile", `
    "-ExecutionPolicy", "Bypass", "-File", "C:\\rc\\entrypoint.ps1"]
CMD ["rc-kube.exe", "serve"]

FROM runner AS codex

COPY assets/openai/ C:/rc/openai/

ENV PATH="C:\rc\openai\node_modules\.bin;${PATH}"

LABEL org.opencontainers.image.title="Windows Codex Workspace runtime" `
    org.opencontainers.image.version="0.147.0"

# Use the native Codex executable so process commands preserve their arguments.
# Batch shims require cmd.exe and cannot preserve arbitrary arguments safely.
RUN $codex = @(Get-ChildItem C:\rc\openai\node_modules\@openai -Recurse -Filter codex.exe); `
    if ($codex.Count -ne 1) { throw 'Expected one Windows Codex executable' }; `
    Copy-Item (Join-Path $codex[0].DirectoryName '*') C:\rc\bin -Recurse -Force; `
    $ripgrep = @(Get-ChildItem C:\rc\openai\node_modules\@openai -Recurse -Filter rg.exe); `
    if ($ripgrep.Count -eq 1) { Copy-Item $ripgrep[0].FullName C:\rc\bin\rg.exe }

RUN & codex.exe --version; `
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }; `
    & pnpm.cmd --version; `
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }; `
    & bun.cmd --version; `
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
