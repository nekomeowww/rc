param(
    [Parameter(Mandatory=$true)][string]$RCBinaryDirectory,
    [string]$NodeVersion = '26.8.1'
)

$ErrorActionPreference = 'Stop'
$downloadDirectory = Join-Path $env:RUNNER_TEMP 'rc-windows-runner-assets'
$nodeArchive = Join-Path $downloadDirectory "node-v$NodeVersion-win-x64.zip"
$nodeDirectory = Join-Path $downloadDirectory "node-v$NodeVersion-win-x64"
$vcRuntimeInstaller = Join-Path $downloadDirectory 'vc_redist.x64.exe'

New-Item -ItemType Directory -Force $downloadDirectory | Out-Null
Invoke-WebRequest `
    -Uri "https://nodejs.org/dist/v$NodeVersion/node-v$NodeVersion-win-x64.zip" `
    -OutFile $nodeArchive
Expand-Archive -LiteralPath $nodeArchive -DestinationPath $downloadDirectory -Force
Invoke-WebRequest -Uri 'https://aka.ms/vs/17/release/vc_redist.x64.exe' -OutFile $vcRuntimeInstaller

& (Join-Path $PSScriptRoot 'prepare-assets.ps1') `
    -RCBinaryDirectory $RCBinaryDirectory `
    -NodeDirectory $nodeDirectory `
    -GitDirectory (Join-Path $env:ProgramFiles 'Git') `
    -VCRuntimeInstaller $vcRuntimeInstaller
