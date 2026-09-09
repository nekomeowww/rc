$ErrorActionPreference = 'Stop'
try {
    # Import the persistent Visual Studio installation's developer environment
    # for every Workspace command. node-gyp can also discover this registered
    # VS 2022 instance directly; importing it makes cl.exe/MSBuild interactive.
    $vsDevCmd = 'C:\BuildTools\Common7\Tools\VsDevCmd.bat'
    if (-not (Test-Path -LiteralPath $vsDevCmd -PathType Leaf)) {
        throw "Missing Visual Studio developer environment: $vsDevCmd"
    }
    $environment = & $env:COMSPEC /d /s /c "call `"$vsDevCmd`" -arch=amd64 -host_arch=amd64 >nul && set"
    if ($LASTEXITCODE -ne 0) { throw 'Could not initialize Visual Studio developer environment' }
    foreach ($line in $environment) {
        if ($line -match '^([^=]+)=(.*)$') {
            Set-Item -LiteralPath "Env:$($matches[1])" -Value $matches[2]
        }
    }
    & (Join-Path $PSScriptRoot 'register-fonts.ps1')
    if ($args.Count -eq 0) { throw 'Specify a command to run' }
    $executable = $args[0]
    $arguments = @($args | Select-Object -Skip 1)
    & $executable @arguments
    if ($null -ne $LASTEXITCODE) { exit $LASTEXITCODE }
    exit 0
} catch {
    Write-Error $_
    exit 1
}
