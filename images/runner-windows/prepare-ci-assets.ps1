param(
    [Parameter(Mandatory=$true)][string]$RCBinaryDirectory,
    [string]$NodeVersion = '26.8.1',
    [string]$NodeSHA256 = '57693d8e93d1b04e7b7de46aca53ecd63e97564e73de36a68428d7ff08d83587',
    [string]$PythonVersion = '3.13.15',
    [string]$PythonSHA256 = 'edec09c4853aeae9ac36efb8c9f95b6b8e2fee65eee56d9767a8b7c69c574403',
    [string]$CMakeVersion = '4.3.5',
    [string]$CMakeSHA256 = 'dac5ddcd2d58699ebe1211173afabfe6f0ca24340e2e995f333cb3e00cff72d6'
)

$ErrorActionPreference = 'Stop'
$downloadDirectory = Join-Path $env:RUNNER_TEMP 'rc-windows-runner-assets'
$nodeArchive = Join-Path $downloadDirectory "node-v$NodeVersion-win-x64.zip"
$nodeDirectory = Join-Path $downloadDirectory "node-v$NodeVersion-win-x64"
$pythonInstaller = Join-Path $downloadDirectory "python-$PythonVersion-amd64.exe"
$cmakeInstaller = Join-Path $downloadDirectory "cmake-$CMakeVersion-windows-x86_64.msi"
$vsBuildToolsBootstrapper = Join-Path $downloadDirectory 'vs_buildtools.exe'
$vcRuntimeInstaller = Join-Path $downloadDirectory 'vc_redist.x64.exe'

New-Item -ItemType Directory -Force $downloadDirectory | Out-Null
function Get-VerifiedAsset($uri, $destination, $sha256) {
    Invoke-WebRequest -Uri $uri -OutFile $destination
    $actual = (Get-FileHash -LiteralPath $destination -Algorithm SHA256).Hash
    if ($actual -ne $sha256) {
        throw "SHA-256 mismatch for ${uri}: expected $sha256, got $actual"
    }
}
function Assert-MicrosoftSignedAsset($path) {
    $signature = Get-AuthenticodeSignature -LiteralPath $path
    if ($signature.Status -ne 'Valid' -or $signature.SignerCertificate.Subject -notmatch 'Microsoft Corporation') {
        throw "Expected a valid Microsoft signature on ${path}: $($signature.Status)"
    }
}
Get-VerifiedAsset `
    "https://nodejs.org/dist/v$NodeVersion/node-v$NodeVersion-win-x64.zip" `
    $nodeArchive `
    $NodeSHA256
Expand-Archive -LiteralPath $nodeArchive -DestinationPath $downloadDirectory -Force
Get-VerifiedAsset `
    "https://www.python.org/ftp/python/$PythonVersion/python-$PythonVersion-amd64.exe" `
    $pythonInstaller `
    $PythonSHA256
Get-VerifiedAsset `
    "https://github.com/Kitware/CMake/releases/download/v$CMakeVersion/cmake-$CMakeVersion-windows-x86_64.msi" `
    $cmakeInstaller `
    $CMakeSHA256
# Microsoft publishes the VS 2022 bootstrapper only through a mutable official
# release channel. The requested component set is stable, but its servicing
# release can advance between builds.
Invoke-WebRequest -Uri 'https://aka.ms/vs/17/release/vs_buildtools.exe' -OutFile $vsBuildToolsBootstrapper
Invoke-WebRequest -Uri 'https://aka.ms/vs/17/release/vc_redist.x64.exe' -OutFile $vcRuntimeInstaller
Assert-MicrosoftSignedAsset $vsBuildToolsBootstrapper
Assert-MicrosoftSignedAsset $vcRuntimeInstaller

& (Join-Path $PSScriptRoot 'prepare-assets.ps1') `
    -RCBinaryDirectory $RCBinaryDirectory `
    -NodeDirectory $nodeDirectory `
    -GitDirectory (Join-Path $env:ProgramFiles 'Git') `
    -PythonInstaller $pythonInstaller `
    -CMakeInstaller $cmakeInstaller `
    -VSBuildToolsBootstrapper $vsBuildToolsBootstrapper `
    -VCRuntimeInstaller $vcRuntimeInstaller
