# install.ps1 — install the PayCLI `pay` binary on Windows (PowerShell 5.1+).
#
# Usage:
#   irm https://raw.githubusercontent.com/KLIXPERT-io/pay-cli/main/install.ps1 | iex
#
#   # with arguments, download first:
#   irm https://raw.githubusercontent.com/KLIXPERT-io/pay-cli/main/install.ps1 -OutFile install.ps1
#   .\install.ps1 -Version v0.1.0 -WithSkills
#
# Environment:
#   $env:PAY_VERSION       same as -Version
#   $env:INSTALL_DIR       same as -Dir
#   $env:GH_TOKEN          used for the api.github.com call (the anonymous API
#                          is 60 req/h per IP)
#   $env:PAY_INSTALL_JSON  when "1", print {"version":…,"path":…} as the last line

[CmdletBinding()]
param(
    [string] $Version,
    [string] $Dir,
    [switch] $WithSkills,
    [switch] $DryRun
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$Repo = 'KLIXPERT-io/pay-cli'
$Bin  = 'pay'

if (-not $Version -and $env:PAY_VERSION) { $Version = $env:PAY_VERSION }
if (-not $Dir     -and $env:INSTALL_DIR) { $Dir     = $env:INSTALL_DIR }

# TLS 1.2 is not the default on Windows PowerShell 5.1, and GitHub requires it.
try {
    [Net.ServicePointManager]::SecurityProtocol =
        [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
} catch { }

# --- architecture ----------------------------------------------------------
# Real detection, not a hardcoded amd64: windows/arm64 ships.
function Get-PayArch {
    $procArch = $env:PROCESSOR_ARCHITECTURE
    $wow64    = $env:PROCESSOR_ARCHITEW6432
    foreach ($a in @($wow64, $procArch)) {
        switch -Regex ($a) {
            '^(ARM64)$'        { return 'arm64' }
            '^(AMD64|x86_64)$' { return 'amd64' }
        }
    }
    if ([Environment]::Is64BitOperatingSystem) { return 'amd64' }
    throw "unsupported architecture: $procArch (pay ships windows/amd64 and windows/arm64)"
}
$arch = Get-PayArch

# --- version ---------------------------------------------------------------
$headers = @{ 'User-Agent' = 'pay-installer' }
if ($env:GH_TOKEN) { $headers['Authorization'] = "Bearer $($env:GH_TOKEN)" }

if (-not $Version) {
    try {
        $latest = Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest" `
                                    -Headers $headers -UseBasicParsing
    } catch {
        throw "could not reach the GitHub release API. Set `$env:GH_TOKEN if you are rate-limited. ($_)"
    }
    $Version = $latest.tag_name
    if (-not $Version) { throw "could not resolve the latest release tag" }
}
if ($Version -notmatch '^v') { $Version = "v$Version" }

$verNoV = $Version.TrimStart('v')
$os     = 'windows'
# FROZEN archive name — must match .goreleaser.yaml's name_template,
# internal/update.archiveAssetName and install.sh. scripts/arch-lint.sh asserts
# all four agree.
$archive = "${Bin}_${verNoV}_${os}_${arch}.zip"
$baseUrl = if ($env:PAY_DOWNLOAD_BASE) { $env:PAY_DOWNLOAD_BASE }
           else { "https://github.com/$Repo/releases/download/$Version" }

# --- target directory ------------------------------------------------------
if (-not $Dir) { $Dir = Join-Path $env:LOCALAPPDATA 'Programs\pay' }
$installPath = Join-Path $Dir "$Bin.exe"

Write-Host "pay $Version  ($os/$arch)"
Write-Host "  archive: $baseUrl/$archive"
Write-Host "  install: $installPath"

if ($DryRun) {
    if ($WithSkills) { Write-Host "  then: pay skills install" }
    exit 0
}

New-Item -ItemType Directory -Force -Path $Dir | Out-Null

# --- download + verify + extract -------------------------------------------
$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("pay-install-" + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Force -Path $tmp | Out-Null
try {
    Write-Host "Downloading $archive ..."
    Invoke-WebRequest -Uri "$baseUrl/$archive"      -OutFile (Join-Path $tmp $archive)        -UseBasicParsing
    Invoke-WebRequest -Uri "$baseUrl/checksums.txt" -OutFile (Join-Path $tmp 'checksums.txt') -UseBasicParsing

    Write-Host "Verifying checksum ..."
    $escaped      = [regex]::Escape($archive)
    $expectedLine = Get-Content (Join-Path $tmp 'checksums.txt') |
                    Where-Object { $_ -match "\s$escaped$" }
    if (-not $expectedLine) { throw "no checksum entry for $archive in checksums.txt" }
    $expected = ($expectedLine -split '\s+')[0].ToLower()
    $actual   = (Get-FileHash -Algorithm SHA256 -Path (Join-Path $tmp $archive)).Hash.ToLower()
    if ($expected -ne $actual) {
        throw "CHECKSUM MISMATCH for $archive`n  expected $expected`n  computed $actual`nRefusing to install."
    }

    # Optional signature verification when cosign is on PATH. Its absence is
    # "no evidence either way", not a pass: the message says which happened.
    $cosign = Get-Command cosign -ErrorAction SilentlyContinue
    if ($cosign) {
        $sigOk = $true
        try {
            Invoke-WebRequest -Uri "$baseUrl/checksums.txt.sig" -OutFile (Join-Path $tmp 'checksums.txt.sig') -UseBasicParsing
            Invoke-WebRequest -Uri "$baseUrl/checksums.txt.pem" -OutFile (Join-Path $tmp 'checksums.txt.pem') -UseBasicParsing
        } catch { $sigOk = $false; Write-Warning "no signature published for this release; checksum only." }
        if ($sigOk) {
            Write-Host "Verifying signature with cosign ..."
            & cosign verify-blob `
                --certificate (Join-Path $tmp 'checksums.txt.pem') `
                --signature   (Join-Path $tmp 'checksums.txt.sig') `
                --certificate-identity-regexp "^https://github.com/$Repo/" `
                --certificate-oidc-issuer "https://token.actions.githubusercontent.com" `
                (Join-Path $tmp 'checksums.txt') | Out-Null
            if ($LASTEXITCODE -ne 0) {
                throw "SIGNATURE VERIFICATION FAILED for checksums.txt. Refusing to install."
            }
            Write-Host "  signature OK"
        }
    } else {
        Write-Warning "cosign is not installed, so only the checksum was verified."
    }

    Expand-Archive -Path (Join-Path $tmp $archive) -DestinationPath $tmp -Force
    $exe = Join-Path $tmp "$Bin.exe"
    if (-not (Test-Path $exe)) { throw "binary $Bin.exe not found inside $archive" }

    # Replacing a running pay.exe fails with a sharing violation; move the old
    # one aside first. `pay` sweeps pay.exe.old on its next start.
    if (Test-Path $installPath) {
        try { Move-Item -Force -Path $installPath -Destination "$installPath.old" } catch { }
    }
    Move-Item -Force -Path $exe -Destination $installPath

    Write-Host ""
    Write-Host "Installed $Version to $installPath"
    try { & $installPath --version } catch { }

    if ($WithSkills) {
        Write-Host ""
        Write-Host "Installing the agent skill ..."
        try { & $installPath skills install } catch { Write-Warning "pay skills install failed; run it manually." }
    }

    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    if (-not ($userPath -split ';' | Where-Object { $_ -ieq $Dir })) {
        Write-Host ""
        Write-Host "Note: $Dir is not on your user PATH. Add it with:"
        Write-Host "  setx PATH `"$Dir;`$env:PATH`""
    }

    if ($env:PAY_INSTALL_JSON -eq '1') {
        $json = @{ version = $Version; path = $installPath; os = $os; arch = $arch } |
                ConvertTo-Json -Compress
        Write-Output $json
    }
}
finally {
    Remove-Item -Recurse -Force -Path $tmp -ErrorAction SilentlyContinue
}
