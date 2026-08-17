<#
.SYNOPSIS
Builds and downloads the official v2rayA release matrix plus portable archives.

.DESCRIPTION
Dispatches the build-only workflow for an already-pushed commit or tag, waits
for it, and downloads the verified assets to the E-drive tools directory. It
does not create tags or publish a GitHub Release.

.EXAMPLE
.\scripts\build-multiplatform-release.ps1 2.4.11.2

.EXAMPLE
.\scripts\build-multiplatform-release.ps1 -Version 2.4.11.3 -Ref v2.4.11-custom.3
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true, Position = 0)]
    [ValidatePattern('^\d+\.\d+\.\d+(\.\d+)?$')]
    [string]$Version,

    [string]$Ref,

    [string]$OutputDirectory
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$repoRoot = Split-Path -Parent $PSScriptRoot
$toolsRoot = Split-Path -Parent $repoRoot
$workspaceRoot = Split-Path -Parent $toolsRoot
$workflow = 'build_multiplatform_portables.yml'

if (-not $Ref) {
    $Ref = (& git -C $repoRoot branch --show-current).Trim()
}
if (-not $Ref) {
    throw 'The repository is in detached HEAD state. Pass -Ref explicitly.'
}

if (-not $OutputDirectory) {
    $OutputDirectory = Join-Path $toolsRoot "v2raya-release-artifacts\v$Version"
}
$OutputDirectory = [System.IO.Path]::GetFullPath($OutputDirectory)
if (Test-Path -LiteralPath $OutputDirectory) {
    $existingOutput = @(Get-ChildItem -LiteralPath $OutputDirectory -Force)
    if ($existingOutput.Count -ne 0) {
        throw "Output directory is not empty: $OutputDirectory"
    }
}

$worktreeChanges = @(& git -C $repoRoot status --porcelain=v1 --untracked-files=all)
if ($LASTEXITCODE -ne 0) {
    throw 'Unable to read the Git worktree status.'
}
$releaseChanges = @($worktreeChanges | Where-Object {
    $_ -notmatch '^\?\? \.serena(?:/|$)'
})
if ($releaseChanges.Count -ne 0) {
    throw 'The worktree has uncommitted files other than local .serena data. Commit them before starting a release build.'
}

$headSha = (& git -C $repoRoot rev-parse HEAD).Trim()
$remoteUrl = (& git -C $repoRoot remote get-url fork).Trim()
if ($remoteUrl -notmatch 'github\.com[/:](?<slug>[^/]+/[^/]+?)(?:\.git)?$') {
    throw "Cannot derive a GitHub repository from fork remote: $remoteUrl"
}
$repository = $Matches.slug

$remoteLines = @(& git -C $repoRoot ls-remote fork `
    "refs/heads/$Ref" "refs/tags/$Ref" "refs/tags/$Ref^{}")
if ($LASTEXITCODE -ne 0) {
    throw "Unable to resolve fork ref $Ref."
}
$remoteEntries = @($remoteLines | ForEach-Object {
    $parts = $_ -split '\s+', 2
    [pscustomobject]@{ Sha = $parts[0]; Name = $parts[1] }
})
$resolvedEntries = @($remoteEntries | Where-Object {
    $_.Name -eq "refs/heads/$Ref" -or
    $_.Name -eq "refs/tags/$Ref^{}" -or
    ($_.Name -eq "refs/tags/$Ref" -and
        "refs/tags/$Ref^{}" -notin $remoteEntries.Name)
})
$remoteShas = @($resolvedEntries.Sha | Sort-Object -Unique)
if ($remoteShas.Count -eq 0) {
    throw "Remote branch or tag fork/$Ref does not exist."
}
if ($remoteShas.Count -ne 1) {
    throw "fork/$Ref is ambiguous because its branch and tag resolve to different commits."
}
$remoteSha = $remoteShas[0]
if ($remoteSha -ne $headSha) {
    throw "fork/$Ref points to $remoteSha, but the local HEAD is $headSha. Push the exact commit first."
}

$ghCandidates = @(
    (Join-Path $repoRoot '.build-tools\github-cli\bin\gh.exe'),
    (Join-Path $workspaceRoot '.toolchains\github-cli\bin\gh.exe'),
    (Join-Path $workspaceRoot '.toolchains\github-cli-2.97.0\bin\gh.exe'),
    (Join-Path $toolsRoot '.build-tools\github-cli\bin\gh.exe'),
    (Join-Path $toolsRoot 'v2raya-anytls-fix\.build-tools\github-cli-2.97.0\bin\gh.exe'),
    (Join-Path $toolsRoot 'v2raya-anytls-fix\.build-tools\github-cli\bin\gh.exe')
)
$ghPath = $ghCandidates | Where-Object { Test-Path -LiteralPath $_ } | Select-Object -First 1
if (-not $ghPath) {
    $toolRoots = @(
        (Join-Path $workspaceRoot '.toolchains'),
        (Join-Path $repoRoot '.build-tools')
    )
    $ghPath = $toolRoots |
        Where-Object { Test-Path -LiteralPath $_ } |
        ForEach-Object {
            Get-ChildItem -LiteralPath $_ -Directory -Filter 'github-cli*' -ErrorAction SilentlyContinue
        } |
        ForEach-Object { Join-Path $_.FullName 'bin\gh.exe' } |
        Where-Object { Test-Path -LiteralPath $_ } |
        Select-Object -First 1
}
if (-not $ghPath) {
    $ghCommand = Get-Command gh -ErrorAction SilentlyContinue
    if ($ghCommand) {
        $ghPath = $ghCommand.Source
    }
}
if (-not $ghPath) {
    throw "GitHub CLI was not found under $workspaceRoot\.toolchains, repository .build-tools, or on PATH."
}

$gitCommand = Get-Command git -ErrorAction Stop
$temporaryToken = $false
$previousToken = $env:GH_TOKEN

try {
    if (-not $env:GH_TOKEN) {
        & $ghPath auth status --hostname github.com *> $null
        if ($LASTEXITCODE -ne 0) {
            $credentialRequest = "protocol=https`nhost=github.com`n`n"
            $credentialLines = @($credentialRequest | & $gitCommand.Source credential fill 2>$null)
            $passwordLine = $credentialLines | Where-Object { $_ -like 'password=*' } | Select-Object -First 1
            if (-not $passwordLine) {
                throw 'GitHub CLI is not authenticated and Git Credential Manager returned no GitHub credential.'
            }
            $env:GH_TOKEN = $passwordLine.Substring('password='.Length)
            $temporaryToken = $true
        }
    }

    & $ghPath workflow view $workflow --repo $repository *> $null
    if ($LASTEXITCODE -ne 0) {
        throw "Workflow $workflow is not registered on the fork default branch. Merge it there before starting a build."
    }

    $existingJson = & $ghPath @(
        'run', 'list',
        '--repo', $repository,
        '--workflow', $workflow,
        '--event', 'workflow_dispatch',
        '--limit', '100',
        '--json', 'databaseId'
    )
    if ($LASTEXITCODE -ne 0) {
        throw 'Unable to list existing workflow runs.'
    }
    $existingIds = @($existingJson | ConvertFrom-Json | ForEach-Object { [long]$_.databaseId })

    & $ghPath @(
        'workflow', 'run', $workflow,
        '--repo', $repository,
        '--ref', $Ref,
        '--field', "version=$Version"
    )
    if ($LASTEXITCODE -ne 0) {
        throw 'Unable to dispatch the multi-platform build workflow.'
    }

    $run = $null
    $deadline = (Get-Date).AddMinutes(2)
    do {
        Start-Sleep -Seconds 2
        $runsJson = & $ghPath @(
            'run', 'list',
            '--repo', $repository,
            '--workflow', $workflow,
            '--event', 'workflow_dispatch',
            '--limit', '20',
            '--json', 'databaseId,headSha,displayTitle,url,createdAt,status'
        )
        if ($LASTEXITCODE -ne 0) {
            throw 'Unable to locate the dispatched workflow run.'
        }
        $run = @($runsJson | ConvertFrom-Json) |
            Where-Object {
                $_.headSha -eq $headSha -and
                $_.displayTitle -eq "Build $Version from $Ref" -and
                [long]$_.databaseId -notin $existingIds
            } |
            Sort-Object createdAt -Descending |
            Select-Object -First 1
    } while (-not $run -and (Get-Date) -lt $deadline)

    if (-not $run) {
        throw 'The workflow was dispatched, but its run ID was not visible within two minutes.'
    }

    Write-Host "Watching GitHub Actions run $($run.databaseId): $($run.url)"
    & $ghPath run watch $run.databaseId --repo $repository --exit-status
    if ($LASTEXITCODE -ne 0) {
        throw "GitHub Actions run $($run.databaseId) failed."
    }

    if (-not (Test-Path -LiteralPath $OutputDirectory)) {
        New-Item -ItemType Directory -Path $OutputDirectory | Out-Null
    }

    & $ghPath @(
        'run', 'download', [string]$run.databaseId,
        '--repo', $repository,
        '--name', "release-assets-$Version",
        '--dir', $OutputDirectory
    )
    if ($LASTEXITCODE -ne 0) {
        throw 'Unable to download the completed release asset artifact.'
    }

    $allFiles = @(Get-ChildItem -LiteralPath $OutputDirectory -File)
    $checksumFiles = @($allFiles | Where-Object Name -Like '*.sha256.txt')
    $assets = @($allFiles | Where-Object Name -NotLike '*.sha256.txt')
    if ($assets.Count -ne 84 -or $checksumFiles.Count -ne 84) {
        throw "Expected 84 assets and 84 checksums; found $($assets.Count) assets and $($checksumFiles.Count) checksums."
    }

    $targets = @(
        @{ Name = 'linux_x64'; Archive = 'tar.gz'; Executable = '' },
        @{ Name = 'linux_arm64'; Archive = 'tar.gz'; Executable = '' },
        @{ Name = 'linux_x86'; Archive = 'tar.gz'; Executable = '' },
        @{ Name = 'linux_riscv64'; Archive = 'tar.gz'; Executable = '' },
        @{ Name = 'linux_mips64'; Archive = 'tar.gz'; Executable = '' },
        @{ Name = 'linux_mips64le'; Archive = 'tar.gz'; Executable = '' },
        @{ Name = 'linux_mips32le'; Archive = 'tar.gz'; Executable = '' },
        @{ Name = 'linux_mips32'; Archive = 'tar.gz'; Executable = '' },
        @{ Name = 'linux_loongarch64'; Archive = 'tar.gz'; Executable = '' },
        @{ Name = 'linux_armv7'; Archive = 'tar.gz'; Executable = '' },
        @{ Name = 'windows_x64'; Archive = 'zip'; Executable = '.exe' },
        @{ Name = 'windows_arm64'; Archive = 'zip'; Executable = '.exe' },
        @{ Name = 'darwin_x64'; Archive = 'tar.gz'; Executable = '' },
        @{ Name = 'darwin_arm64'; Archive = 'tar.gz'; Executable = '' },
        @{ Name = 'freebsd_x64'; Archive = 'tar.gz'; Executable = '' },
        @{ Name = 'freebsd_arm64'; Archive = 'tar.gz'; Executable = '' },
        @{ Name = 'openbsd_x64'; Archive = 'tar.gz'; Executable = '' },
        @{ Name = 'openbsd_arm64'; Archive = 'tar.gz'; Executable = '' }
    )
    $expectedAssets = foreach ($target in $targets) {
        "v2raya_$($target.Name)_$Version$($target.Executable)"
        "v2raya_core_$($target.Name)_$Version$($target.Executable)"
        $portableTarget = $target.Name.Replace('_', '-')
        "v2raya-$Version-$portableTarget-portable.$($target.Archive)"
    }
    $linuxArchitectures = @(
        'x64', 'arm64', 'x86', 'riscv64', 'mips64', 'mips64le',
        'mips32le', 'mips32', 'loongarch64', 'armv7'
    )
    foreach ($architecture in $linuxArchitectures) {
        $expectedAssets += "installer_debian_${architecture}_$Version.deb"
        $expectedAssets += "installer_redhat_${architecture}_$Version.rpm"
    }
    $archLinuxArchitectures = @(
        'x64', 'arm64', 'x86', 'riscv64', 'loongarch64', 'armv7'
    )
    foreach ($architecture in $archLinuxArchitectures) {
        $expectedAssets += "installer_archlinux_${architecture}_$Version.pkg.tar.zst"
    }
    $expectedAssets += @(
        "installer_windows_inno_x64_$Version.exe",
        "installer_windows_inno_arm64_$Version.exe",
        'web.tar.gz',
        'web.zip'
    )
    $actualAssetNames = @($assets.Name)
    $missingAssets = @($expectedAssets | Where-Object { $_ -notin $actualAssetNames })
    $unexpectedAssets = @($actualAssetNames | Where-Object { $_ -notin $expectedAssets })
    if ($missingAssets.Count -ne 0 -or $unexpectedAssets.Count -ne 0) {
        throw "Release asset names do not match the official matrix. Missing: $($missingAssets -join ', '). Unexpected: $($unexpectedAssets -join ', ')."
    }

    foreach ($asset in $assets) {
        $checksumPath = "$($asset.FullName).sha256.txt"
        if (-not (Test-Path -LiteralPath $checksumPath)) {
            throw "Missing checksum for $($asset.Name)."
        }
        $expected = (Get-Content -LiteralPath $checksumPath -Raw).Trim().ToLowerInvariant()
        if ($expected -notmatch '^[0-9a-f]{64}$') {
            throw "Invalid SHA256 sidecar for $($asset.Name)."
        }
        $actual = (Get-FileHash -LiteralPath $asset.FullName -Algorithm SHA256).Hash.ToLowerInvariant()
        if ($expected -ne $actual) {
            throw "SHA256 mismatch for $($asset.Name)."
        }
    }

    Write-Host "Verified 36 binaries, 28 installers, 2 Web archives, 18 portable archives and 84 SHA256 files."
    Write-Host "Output: $OutputDirectory"
} finally {
    if ($temporaryToken) {
        if ($null -eq $previousToken) {
            Remove-Item Env:GH_TOKEN -ErrorAction SilentlyContinue
        } else {
            $env:GH_TOKEN = $previousToken
        }
    }
}
