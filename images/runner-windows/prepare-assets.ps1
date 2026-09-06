param(
    [Parameter(Mandatory=$true)][string]$RCBinaryDirectory,
    [Parameter(Mandatory=$true)][string]$NodeDirectory,
    [Parameter(Mandatory=$true)][string]$GitDirectory,
    [string]$OpenaiDirectory,
    [Parameter(Mandatory=$true)][string]$VCRuntimeInstaller,
    [string]$FontDirectory = "$env:WINDIR\Fonts"
)
$ErrorActionPreference = 'Stop'
$required = @(
    (Join-Path $RCBinaryDirectory 'rc-kube.exe'),
    (Join-Path $RCBinaryDirectory 'rcctl.exe'),
    (Join-Path $NodeDirectory 'node.exe'),
    (Join-Path $GitDirectory 'cmd\git.exe'),
    $VCRuntimeInstaller
)
if ($OpenaiDirectory) {
    $required += Join-Path $OpenaiDirectory 'node_modules\.bin\codex.cmd'
}
foreach ($file in $required) {
    if (-not (Test-Path -LiteralPath $file -PathType Leaf)) { throw "Missing asset: $file" }
}
# The caller selects the Node distribution, as the Linux image does through
# its base-image tag. Check executability without pinning an experiment patch.
& (Join-Path $NodeDirectory 'node.exe') --version
if ($LASTEXITCODE -ne 0) { throw 'Could not execute the supplied Node binary' }
$assets = Join-Path $PSScriptRoot 'assets'
New-Item -ItemType Directory -Force $assets, (Join-Path $assets 'fonts') | Out-Null
function Copy-AssetTree($source, $name) {
    & robocopy.exe $source (Join-Path $assets $name) /E /R:1 /W:1 /NFL /NDL /NJH /NJS /NP
    if ($LASTEXITCODE -ge 8) { throw "Could not stage $source" }
}
New-Item -ItemType Directory -Force (Join-Path $assets 'rc') | Out-Null
Copy-Item (Join-Path $RCBinaryDirectory 'rc-kube.exe'), (Join-Path $RCBinaryDirectory 'rcctl.exe') (Join-Path $assets 'rc') -Force
Copy-AssetTree $NodeDirectory 'node'
Copy-AssetTree $GitDirectory 'git'
if ($OpenaiDirectory) {
    Copy-AssetTree $OpenaiDirectory 'openai'
}
Copy-Item -LiteralPath $VCRuntimeInstaller -Destination (Join-Path $assets 'vc_redist.x64.exe') -Force
foreach ($file in @('arial.ttf','arialbd.ttf','segoeui.ttf','segoeuib.ttf','tahoma.ttf','tahomabd.ttf','micross.ttf')) {
    Copy-Item -LiteralPath (Join-Path $FontDirectory $file) -Destination (Join-Path $assets "fonts\$file") -Force
}
Write-Output "Prepared image assets in $assets"
exit 0
