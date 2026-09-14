param(
    [Parameter(Mandatory=$true)][string]$RCBinaryDirectory,
    [switch]$IncludeCodex,
    [string]$NodeVersion = '26.8.2',
    [string]$NodeSHA256 = 'cf02f5d0c06c794b84f277177d5cf3743d0924ca49f6641cd435dd7cb6ee9085',
    [string]$PythonVersion = '3.14.7',
    [string]$PythonSHA256 = '9d9eb2709ef81bf5cd30db3c2096bdbc4ea10087c22e62f27d356b36f6ae9649',
    [string]$CMakeVersion = '4.4.3',
    [string]$CMakeSHA256 = 'f3b27c83979727b73540db53dbe610967656b4631746a94b17a1dd0329dd7868'
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

$codexArguments = @{}
if ($IncludeCodex) {
    $dockerfile = Get-Content (Join-Path $PSScriptRoot 'Dockerfile') -Raw
    function Get-ToolVersion([string]$name) {
        $match = [regex]::Match($dockerfile, "(?m)^ARG ${name}_VERSION=([0-9]+\.[0-9]+\.[0-9]+)\r?$")
        if (-not $match.Success) { throw "Missing pinned $name version in Dockerfile" }
        return $match.Groups[1].Value
    }
    $openaiDirectory = Join-Path $downloadDirectory 'openai'
    # Use the downloaded Node, including for npm lifecycle scripts.
    $env:PATH = "$nodeDirectory;$env:PATH"
    & (Join-Path $nodeDirectory 'npm.cmd') install --prefix $openaiDirectory `
        "@openai/codex@$(Get-ToolVersion 'CODEX')" `
        "pnpm@$(Get-ToolVersion 'PNPM')" `
        "bun@$(Get-ToolVersion 'BUN')"
    if ($LASTEXITCODE -ne 0) { throw 'Could not install Windows Codex tools' }
    $codexArguments['OpenaiDirectory'] = $openaiDirectory
}

& (Join-Path $PSScriptRoot 'prepare-assets.ps1') @codexArguments `
    -RCBinaryDirectory $RCBinaryDirectory `
    -NodeDirectory $nodeDirectory `
    -GitDirectory (Join-Path $env:ProgramFiles 'Git') `
    -PythonInstaller $pythonInstaller `
    -CMakeInstaller $cmakeInstaller `
    -VSBuildToolsBootstrapper $vsBuildToolsBootstrapper `
    -VCRuntimeInstaller $vcRuntimeInstaller
