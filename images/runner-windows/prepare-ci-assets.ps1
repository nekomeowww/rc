param(
    [Parameter(Mandatory=$true)][string]$RCBinaryDirectory,
    [switch]$IncludeCodex,
    # renovate: datasource=node-version depName=node
    [string]$NodeVersion = '26.13.1',
    # renovate: datasource=python-version depName=python
    [string]$PythonVersion = '3.14.10',
    # renovate: datasource=github-releases depName=Kitware/CMake extractVersion=^v(?<version>.*)$
    [string]$CMakeVersion = '4.5.1'
)

$ErrorActionPreference = 'Stop'
$downloadDirectory = Join-Path $env:RUNNER_TEMP 'rc-windows-runner-assets'
$nodeFile = "node-v$NodeVersion-win-x64.zip"
$nodeArchive = Join-Path $downloadDirectory $nodeFile
$nodeDirectory = Join-Path $downloadDirectory "node-v$NodeVersion-win-x64"
$pythonFile = "python-$PythonVersion-amd64.exe"
$pythonInstaller = Join-Path $downloadDirectory $pythonFile
$cmakeFile = "cmake-$CMakeVersion-windows-x86_64.msi"
$cmakeInstaller = Join-Path $downloadDirectory $cmakeFile
$vsBuildToolsBootstrapper = Join-Path $downloadDirectory 'vs_buildtools.exe'
$vcRuntimeInstaller = Join-Path $downloadDirectory 'vc_redist.x64.exe'

New-Item -ItemType Directory -Force $downloadDirectory | Out-Null
function Get-Sha256FromList([string]$uri, [string]$fileName) {
    $response = Invoke-WebRequest -Uri $uri -UseBasicParsing
    $content = if ($response.Content -is [byte[]]) {
        [System.Text.Encoding]::UTF8.GetString($response.Content)
    } else {
        [string]$response.Content
    }
    foreach ($line in ($content -split "`r?`n")) {
        if ($line -match "^([0-9a-fA-F]{64})\s+\*?(\./)?$([regex]::Escape($fileName))$") {
            return $Matches[1].ToUpperInvariant()
        }
    }
    throw "Could not find SHA-256 for $fileName in $uri"
}
function Get-PythonSha256FromSpdx([string]$uri) {
    $spdx = Invoke-RestMethod -Uri $uri
    $pkg = @($spdx.packages | Where-Object { $_.name -eq 'CPython' })[0]
    $sum = @($pkg.checksums | Where-Object { $_.algorithm -eq 'SHA256' })[0]
    if (-not $sum -or $sum.checksumValue -notmatch '^[0-9a-fA-F]{64}$') {
        throw "Could not find CPython SHA-256 in $uri"
    }
    return $sum.checksumValue.ToUpperInvariant()
}
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
$nodeSha256 = Get-Sha256FromList "https://nodejs.org/dist/v$NodeVersion/SHASUMS256.txt" $nodeFile
Get-VerifiedAsset `
    "https://nodejs.org/dist/v$NodeVersion/$nodeFile" `
    $nodeArchive `
    $nodeSha256
Expand-Archive -LiteralPath $nodeArchive -DestinationPath $downloadDirectory -Force
$pythonSha256 = Get-PythonSha256FromSpdx "https://www.python.org/ftp/python/$PythonVersion/$pythonFile.spdx.json"
Get-VerifiedAsset `
    "https://www.python.org/ftp/python/$PythonVersion/$pythonFile" `
    $pythonInstaller `
    $pythonSha256
$cmakeSha256 = Get-Sha256FromList "https://github.com/Kitware/CMake/releases/download/v$CMakeVersion/cmake-$CMakeVersion-SHA-256.txt" $cmakeFile
Get-VerifiedAsset `
    "https://github.com/Kitware/CMake/releases/download/v$CMakeVersion/$cmakeFile" `
    $cmakeInstaller `
    $cmakeSha256
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
