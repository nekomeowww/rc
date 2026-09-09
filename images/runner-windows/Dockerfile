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
COPY assets/vc_redist.x64.exe C:/rc/vc_redist.x64.exe
COPY entrypoint.ps1 register-fonts.ps1 C:/rc/

RUN $process = Start-Process C:\rc\python-amd64.exe `
        -ArgumentList '/quiet', 'InstallAllUsers=1', 'TargetDir=C:\Python3', `
            'Include_doc=0', 'Include_debug=0', 'Include_dev=0', `
            'Include_launcher=0', 'Include_pip=0', 'Include_test=0', `
            'PrependPath=0', 'Shortcuts=0' `
        -Wait `
        -PassThru; `
    if ($process.ExitCode -notin @(0, 3010)) { `
        throw "Python installation failed: $($process.ExitCode)" `
    }; `
    $process = Start-Process msiexec.exe `
        -ArgumentList '/i', 'C:\rc\cmake-x64.msi', '/qn', '/norestart', `
            'ADD_CMAKE_TO_PATH=System' `
        -Wait `
        -PassThru; `
    if ($process.ExitCode -notin @(0, 3010)) { `
        throw "CMake installation failed: $($process.ExitCode)" `
    }; `
    $process = Start-Process C:\rc\vs_buildtools.exe `
        -ArgumentList '--quiet', '--wait', '--norestart', '--nocache', `
            '--installPath', 'C:\BuildTools', `
            '--add', 'Microsoft.VisualStudio.Workload.VCTools', `
            '--add', 'Microsoft.VisualStudio.Component.VC.Tools.x86.x64', `
            '--add', 'Microsoft.VisualStudio.Component.Windows11SDK.26100' `
        -Wait `
        -PassThru; `
    if ($process.ExitCode -notin @(0, 3010)) { `
        throw "Visual Studio Build Tools installation failed: $($process.ExitCode)" `
    }; `
    $process = Start-Process C:\rc\vc_redist.x64.exe `
        -ArgumentList '/install', '/quiet', '/norestart' `
        -Wait `
        -PassThru; `
    if ($process.ExitCode -notin @(0, 3010)) { `
        throw "VC runtime installation failed: $($process.ExitCode)" `
    }; `
    Remove-Item C:\rc\python-amd64.exe, C:\rc\cmake-x64.msi, `
        C:\rc\vs_buildtools.exe, C:\rc\vc_redist.x64.exe; `
    if (Test-Path 'C:\ProgramData\Package Cache') { `
        Remove-Item 'C:\ProgramData\Package Cache' -Recurse -Force `
    }; `
    & C:\rc\register-fonts.ps1; `
    New-Item -ItemType Directory -Force C:\workspace, C:\home\agent | Out-Null; `
    foreach ($path in @('C:\workspace', 'C:\home\agent')) { `
        & icacls.exe $path /grant 'ContainerUser:(OI)(CI)F' /T /C /Q; `
        if ($LASTEXITCODE -ne 0) { throw "Could not grant ContainerUser access to $path" } `
    }

COPY assets/rc/ C:/rc/bin/

ENV APPDATA="C:\home\agent\AppData\Roaming" `
    HOME="C:\home\agent" `
    LOCALAPPDATA="C:\home\agent\AppData\Local" `
    NODE_GYP_FORCE_PYTHON="C:\Python3\python.exe" `
    npm_config_msvs_version="2022" `
    npm_config_python="C:\Python3\python.exe" `
    PATH="C:\rc\bin;C:\rc\node;C:\rc\git\cmd;C:\Python3;C:\Program Files\CMake\bin;C:\Windows\system32;C:\Windows;C:\Windows\System32\WindowsPowerShell\v1.0" `
    USERPROFILE="C:\home\agent"

# Verify that unprivileged Workspace processes can resolve the portable tools.
# entrypoint.ps1 loads VsDevCmd for runtime commands so cl.exe and MSBuild are
# also available without hard-coding the servicing-dependent MSVC directory.
USER ContainerUser
RUN & python.exe --version; `
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }; `
    & cmake.exe --version; `
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }; `
    $command = 'call "C:\BuildTools\Common7\Tools\VsDevCmd.bat" -arch=amd64 -host_arch=amd64 >nul && cl.exe /? >nul && MSBuild.exe -version >nul'; `
    & $env:COMSPEC /d /s /c $command; `
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

LABEL org.opencontainers.image.source="https://github.com/nekomeowww/rc" `
    org.opencontainers.image.title="Windows Workspace runner"

WORKDIR C:\workspace

ENTRYPOINT ["powershell.exe", "-NoLogo", "-NoProfile", `
    "-ExecutionPolicy", "Bypass", "-File", "C:\\rc\\entrypoint.ps1"]
CMD ["rc-kube.exe", "serve"]

FROM runner AS codex

USER ContainerAdministrator

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

USER ContainerUser
