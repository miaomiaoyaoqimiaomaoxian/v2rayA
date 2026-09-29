param(
    [Parameter(Mandatory)] [string]$Version,
    [Parameter(Mandatory)] [string]$ManagerExe,
    [Parameter(Mandatory)] [string]$CoreExe,
    [Parameter(Mandatory)] [string]$OfficialInstaller,
    [Parameter(Mandatory)] [string]$OfficialInstallerSha256,
    [Parameter(Mandatory)] [string]$InnoCompiler,
    [Parameter(Mandatory)] [string]$InnoUnpacker,
    [Parameter(Mandatory)] [string]$OutputDirectory
)

$ErrorActionPreference = 'Stop'
if ($Version -notmatch '^[0-9][0-9A-Za-z.-]+$') { throw 'Invalid version string' }
$root = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
$templatePath = Join-Path $PSScriptRoot 'windows.iss'
foreach ($file in @($ManagerExe, $CoreExe, $OfficialInstaller, $InnoCompiler, $InnoUnpacker, $templatePath)) {
    if (-not (Test-Path -LiteralPath $file -PathType Leaf)) { throw "Missing input: $file" }
}
if ((Get-FileHash -LiteralPath $OfficialInstaller -Algorithm SHA256).Hash -ne $OfficialInstallerSha256.ToUpperInvariant()) {
    throw 'Official installer SHA-256 mismatch'
}
$managerVersion = (& $ManagerExe --version | Out-String).Trim()
if ($LASTEXITCODE -ne 0 -or $managerVersion -ne "v$Version") {
    throw "Manager version does not match v$Version"
}

$output = [IO.Path]::GetFullPath($OutputDirectory)
New-Item -ItemType Directory -Path $output -Force | Out-Null
$stage = Join-Path $output "inno-stage-$Version"
if (Test-Path -LiteralPath $stage) { throw "Stage already exists: $stage" }
New-Item -ItemType Directory -Path $stage | Out-Null
$extracted = Join-Path $stage 'official-extracted'
& $InnoUnpacker -x -b -q ('-d' + $extracted) $OfficialInstaller | Out-Null
if ($LASTEXITCODE -ne 0) { throw 'Could not extract official installer resources' }

$package = Join-Path $stage 'v2raya-x64compatible-windows'
$bin = Join-Path $package 'bin'
$data = Join-Path $package 'data'
New-Item -ItemType Directory -Path $bin, $data | Out-Null
Copy-Item -LiteralPath $ManagerExe -Destination (Join-Path $bin 'v2raya.exe')
Copy-Item -LiteralPath $CoreExe -Destination (Join-Path $bin 'v2raya_core.exe')
Copy-Item -LiteralPath (Join-Path $extracted '{app}\bin\wintun.dll') -Destination (Join-Path $bin 'wintun.dll')
foreach ($name in @('geoip.dat', 'geosite.dat', 'LoyalsoldierSite.dat')) {
    Copy-Item -LiteralPath (Join-Path $extracted "{app}\data\$name") -Destination (Join-Path $data $name)
}
Copy-Item -LiteralPath (Join-Path $root 'LICENSE') -Destination (Join-Path $stage 'LICENSE.txt')
Copy-Item -LiteralPath (Join-Path $PSScriptRoot 'v2raya.ico') -Destination (Join-Path $stage 'v2raya.ico')
Copy-Item -LiteralPath (Join-Path $PSScriptRoot 'ChineseSimplified.isl') -Destination (Join-Path $stage 'ChineseSimplified.isl')

# Keep upstream's Inno recipe, changing only its CI placeholders and local staging paths.
$recipe = Get-Content -LiteralPath $templatePath -Raw
$recipe = $recipe.Replace('TheRealVersion', $Version).Replace('TheRealArch', 'x64compatible')
$recipe = $recipe.Replace('D:\', $stage + '\')
$recipe = $recipe.Replace('#define MyAppPublisher "v2rayA Organization"', '#define MyAppPublisher "miaomiaoyaoqimiaomaoxian v2rayA fork"')
$recipe = $recipe.Replace('#define MyAppURL "https://v2raya.org/"', '#define MyAppURL "https://github.com/miaomiaoyaoqimiaomaoxian/v2rayA"')
$recipePath = Join-Path $stage 'windows_x64_custom.iss'
Set-Content -LiteralPath $recipePath -Value $recipe -Encoding UTF8
Push-Location $stage
try {
    & $InnoCompiler $recipePath | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'Inno Setup compile failed' }
} finally {
    Pop-Location
}

$compiled = Join-Path $stage 'installer_windows_inno_x64compatible.exe'
if (-not (Test-Path -LiteralPath $compiled -PathType Leaf)) { throw 'Inno Setup did not produce an installer' }
$verified = Join-Path $stage 'verified-extracted'
& $InnoUnpacker -x -b -q ('-d' + $verified) $compiled | Out-Null
if ($LASTEXITCODE -ne 0) { throw 'Could not verify compiled installer contents' }
foreach ($entry in @(
    @{ source = $ManagerExe; installed = '{app}\bin\v2raya.exe' },
    @{ source = $CoreExe; installed = '{app}\bin\v2raya_core.exe' },
    @{ source = (Join-Path $bin 'wintun.dll'); installed = '{app}\bin\wintun.dll' },
    @{ source = (Join-Path $data 'geoip.dat'); installed = '{app}\data\geoip.dat' },
    @{ source = (Join-Path $data 'geosite.dat'); installed = '{app}\data\geosite.dat' }
)) {
    $actual = Join-Path $verified $entry.installed
    if ((Get-FileHash -LiteralPath $entry.source -Algorithm SHA256).Hash -ne (Get-FileHash -LiteralPath $actual -Algorithm SHA256).Hash) {
        throw "Installer payload mismatch: $($entry.installed)"
    }
}

$asset = Join-Path $output "installer_windows_inno_x64_$Version.exe"
if (Test-Path -LiteralPath $asset) { throw "Release asset already exists: $asset" }
Copy-Item -LiteralPath $compiled -Destination $asset
$item = Get-Item -LiteralPath $asset
[pscustomobject]@{
    version = $Version
    path = $item.FullName
    bytes = $item.Length
    sha256 = (Get-FileHash -LiteralPath $asset -Algorithm SHA256).Hash.ToLowerInvariant()
    manager_sha256 = (Get-FileHash -LiteralPath $ManagerExe -Algorithm SHA256).Hash.ToLowerInvariant()
    core_sha256 = (Get-FileHash -LiteralPath $CoreExe -Algorithm SHA256).Hash.ToLowerInvariant()
} | ConvertTo-Json
