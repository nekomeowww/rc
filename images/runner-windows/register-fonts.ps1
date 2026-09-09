$ErrorActionPreference = 'Stop'
$fontDirectory = Join-Path $PSScriptRoot 'fonts'
$registryPath = 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Fonts'
$fonts = @{
    'Arial (TrueType)' = 'arial.ttf'
    'Arial Bold (TrueType)' = 'arialbd.ttf'
    'Segoe UI (TrueType)' = 'segoeui.ttf'
    'Segoe UI Bold (TrueType)' = 'segoeuib.ttf'
    'Tahoma (TrueType)' = 'tahoma.ttf'
    'Tahoma Bold (TrueType)' = 'tahomabd.ttf'
    'Microsoft Sans Serif (TrueType)' = 'micross.ttf'
}
Add-Type @'
using System.Runtime.InteropServices;
public static class RcExperimentFonts {
    [DllImport("gdi32.dll", CharSet=CharSet.Unicode)]
    public static extern int AddFontResourceW(string path);
}
'@
foreach ($name in $fonts.Keys) {
    $file = $fonts[$name]
    $destination = Join-Path $env:WINDIR "Fonts\$file"
    if (-not (Test-Path $destination)) {
        Copy-Item (Join-Path $fontDirectory $file) $destination
    }
    $registered = Get-ItemPropertyValue $registryPath -Name $name -ErrorAction SilentlyContinue
    if ($registered -ne $file) {
        New-ItemProperty $registryPath -Name $name -Value $file -PropertyType String -Force | Out-Null
    }
    if ([RcExperimentFonts]::AddFontResourceW($destination) -eq 0) {
        throw "Could not load font: $destination"
    }
}
