$ErrorActionPreference = 'Stop'

# Run through entrypoint.ps1 as ContainerUser so this checks the same compiler
# environment and permissions as Workspace commands, not the build host.
# Test both directories using unique files so concurrent probes cannot collide.
foreach ($cache in @('C:\tmp\cache\pnpm\store', 'C:\tmp\cache\pnpm\virtual')) {
    $file = Join-Path $cache ([Guid]::NewGuid().ToString('N'))
    Set-Content -LiteralPath $file -Value 'cache'
    Remove-Item -LiteralPath $file
}

& python.exe --version
if ($LASTEXITCODE -ne 0) { throw 'Python is unavailable' }
& cmake.exe --version
if ($LASTEXITCODE -ne 0) { throw 'CMake is unavailable' }
& MSBuild.exe -version
if ($LASTEXITCODE -ne 0) { throw 'MSBuild is unavailable' }

# Compile and execute a program using the Windows SDK. Version/help probes alone
# do not prove that headers, linker libraries and the VC runtime are installed.
$probe = Join-Path ([System.IO.Path]::GetTempPath()) ('rc-toolchain-' + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $probe | Out-Null
try {
    $source = Join-Path $probe 'probe.cpp'
    $binary = Join-Path $probe 'probe.exe'
    $object = Join-Path $probe 'probe.obj'
    Set-Content -LiteralPath $source -Encoding Ascii -Value @'
#include <windows.h>
#include <iostream>
int main() {
    std::cout << "native-toolchain-ok" << std::endl;
    return GetCurrentProcessId() == 0;
}
'@
    & cl.exe /nologo /EHsc $source "/Fe:$binary" "/Fo:$object"
    if ($LASTEXITCODE -ne 0) { throw 'Windows SDK compile/link failed' }
    & $binary
    if ($LASTEXITCODE -ne 0) { throw 'Native executable failed' }
} finally {
    # Only the unique disposable fixture created above is removed.
    Remove-Item -LiteralPath $probe -Recurse -Force
}
