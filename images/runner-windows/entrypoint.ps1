$ErrorActionPreference = 'Stop'
try {
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
