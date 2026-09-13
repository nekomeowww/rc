$ErrorActionPreference = 'Stop'

# Keep installer argument arrays in a file: Windows command-line re-quoting of
# a long Docker RUN command can otherwise hide the original installer error.
function Install-Tool([string]$Executable, [string[]]$Arguments) {
    Write-Output "Installing $Executable"
    # All installers use the same wait/exit handling; retain logs on failure
    # instead of polling VS-specific progress during successful installations.
    $process = Start-Process $Executable -ArgumentList $Arguments -Wait -PassThru
    if ($process.ExitCode -notin @(0, 3010)) {
        # Failed build containers are disposable; retain the installer diagnosis
        # in build output before the snapshot becomes inaccessible.
        Get-ChildItem $env:TEMP -Filter 'dd_*.log' -File |
            Sort-Object LastWriteTime -Descending | Select-Object -First 3 |
            ForEach-Object { Write-Output $_.Name; Get-Content $_.FullName -Tail 50 }
        throw "Installation failed for ${Executable}: $($process.ExitCode)"
    }
}

Install-Tool 'C:\rc\python-amd64.exe' @(
    '/quiet', 'InstallAllUsers=1', 'TargetDir=C:\Python3',
    'Include_doc=0', 'Include_debug=0', 'Include_dev=0', 'Include_launcher=0',
    'Include_pip=0', 'Include_test=0', 'PrependPath=0', 'Shortcuts=0'
)
Install-Tool 'msiexec.exe' @(
    '/i', 'C:\rc\cmake-x64.msi', '/qn', '/norestart', 'ADD_CMAKE_TO_PATH=System'
)
$vsInstaller = 'C:\rc\vs_buildtools.exe'
$vsArguments = @(
    '--quiet', '--wait', '--norestart', '--nocache', '--installPath', 'C:\BuildTools',
    '--add', 'Microsoft.VisualStudio.Workload.VCTools',
    '--add', 'Microsoft.VisualStudio.Component.VC.Tools.x86.x64',
    '--add', 'Microsoft.VisualStudio.Component.Windows11SDK.26100'
)
# A downloaded layout permits BuildKit workers without build-step networking.
# https://learn.microsoft.com/visualstudio/install/create-an-offline-installation-of-visual-studio
if (Test-Path 'C:\rc\vs-layout\vs_buildtools.exe' -PathType Leaf) {
    # NOTICE: Offline containers cannot refresh signing roots via Windows Update.
    # Import only the two pinned Microsoft roots supplied by the official layout;
    # never disable installer signature verification or trust arbitrary roots.
    # https://learn.microsoft.com/visualstudio/install/install-certificates-for-visual-studio-offline
    $roots = @{
        'manifestRootCertificate.cer' = '8F43288AD272F3103B6FB1428485EA3014C0BCFE'
        'manifestCounterSignRootCertificate.cer' = '3B1EFD3A66EA28B16697394703A72CA340A05BD5'
    }
    foreach ($name in $roots.Keys) {
        $file = Join-Path 'C:\rc\vs-layout\Certificates' $name
        $certificate = New-Object System.Security.Cryptography.X509Certificates.X509Certificate2($file)
        if ($certificate.Thumbprint -ne $roots[$name]) { throw "Unexpected Microsoft root: $name" }
        Import-Certificate -FilePath $file -CertStoreLocation Cert:\LocalMachine\Root | Out-Null
    }
    # Recent installers also require this intermediate, which older layouts
    # omit. It belongs in CA, not Root; retain normal chain verification.
    # https://learn.microsoft.com/troubleshoot/developer/visualstudio/installation/install-failure-2017-later-versions
    $signingCA = 'C:\rc\vs-layout\Certificates\MicrosoftWindowsCodeSigningPCA2024.crt'
    if ((Get-FileHash $signingCA -Algorithm SHA256).Hash -ne 'FE229EFC927F6D77B738896752A21803A59736BAA17BF5BE9A50C72E219CBCD2') {
        throw 'Unexpected Microsoft Windows Code Signing PCA 2024 certificate'
    }
    Import-Certificate -FilePath $signingCA -CertStoreLocation Cert:\LocalMachine\CA | Out-Null
    $vsInstaller = 'C:\rc\vs-layout\vs_buildtools.exe'
    $vsArguments += '--noWeb'
}
Install-Tool $vsInstaller $vsArguments
Install-Tool 'C:\rc\vc_redist.x64.exe' @('/install', '/quiet', '/norestart')

# These are staged installers in the image, never host directories. Servicing
# the toolchain is done by rebuilding the image, not modifying a live Workspace.
Remove-Item C:\rc\python-amd64.exe, C:\rc\cmake-x64.msi, C:\rc\vs_buildtools.exe, C:\rc\vc_redist.x64.exe
Remove-Item C:\rc\vs-layout -Recurse -Force
if (Test-Path 'C:\ProgramData\Package Cache') {
    Remove-Item 'C:\ProgramData\Package Cache' -Recurse -Force
}
# Keep rebuildable caches off persistent home, with the same ContainerUser
# permissions as the workspace. Runtime replacement can discard these paths.
foreach ($path in @('C:\workspace', 'C:\home\agent', 'C:\tmp\cache')) {
    New-Item -ItemType Directory -Force $path | Out-Null
    & icacls.exe $path /grant '*S-1-5-93-2-2:(OI)(CI)F' /T /C /Q
    if ($LASTEXITCODE -ne 0) { throw 'Could not grant ContainerUser path access' }
}
New-Item -ItemType Directory -Force C:\tmp\cache\pnpm\store, C:\tmp\cache\pnpm\virtual | Out-Null
