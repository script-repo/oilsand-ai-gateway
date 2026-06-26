<#
.SYNOPSIS
  One-line installer for the oilsand-ai-gateway TUI (Windows).

.DESCRIPTION
  Downloads the latest GitHub release archive for your architecture and extracts
  the binary together with the bundled scripts/ helpers (so Nutanix deploy works).

.EXAMPLE
  irm https://raw.githubusercontent.com/script-repo/oilsand-ai-gateway/main/scripts/install.ps1 | iex

.NOTES
  Override with env vars before running:
    $env:OILSAND_VERSION     = 'v1.2.3'   # pin a release (default: latest)
    $env:OILSAND_INSTALL_DIR = 'C:\tools\oilsand'
#>
$ErrorActionPreference = 'Stop'

$Repo    = 'script-repo/oilsand-ai-gateway'
$BinName = 'oilsand-tui'
$InstallDir = if ($env:OILSAND_INSTALL_DIR) { $env:OILSAND_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'oilsand-tui' }

function Info($m) { Write-Host "[install] $m" }
function Fail($m) { Write-Error "[install] ERROR: $m"; exit 1 }

# Map architecture -> GoReleaser arch.
$archRaw = $env:PROCESSOR_ARCHITECTURE
switch ($archRaw) {
  'AMD64' { $Arch = 'amd64' }
  'ARM64' { $Arch = 'arm64' }
  default { Fail "unsupported architecture: $archRaw" }
}

# Resolve version (latest unless pinned).
$Version = $env:OILSAND_VERSION
if (-not $Version) {
  Info 'resolving latest release'
  try {
    $rel = Invoke-RestMethod -Headers @{ 'User-Agent' = 'oilsand-install' } `
      -Uri "https://api.github.com/repos/$Repo/releases/latest"
    $Version = $rel.tag_name
  } catch {
    Fail "could not determine latest release; set `$env:OILSAND_VERSION ($_)"
  }
}
$VerNoV = $Version.TrimStart('v')

$Asset = "${BinName}_${VerNoV}_windows_${Arch}.zip"
$Url   = "https://github.com/$Repo/releases/download/$Version/$Asset"

$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("oilsand-" + [System.Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $tmp -Force | Out-Null
try {
  $zip = Join-Path $tmp $Asset
  Info "downloading $Asset ($Version)"
  Invoke-WebRequest -Headers @{ 'User-Agent' = 'oilsand-install' } -Uri $Url -OutFile $zip

  Info "installing to $InstallDir"
  if (Test-Path $InstallDir) { Remove-Item -Recurse -Force $InstallDir }
  New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
  Expand-Archive -Path $zip -DestinationPath $InstallDir -Force
} finally {
  Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}

$exe = Join-Path $InstallDir "$BinName.exe"
if (-not (Test-Path $exe)) { Fail "binary not found after extraction: $exe" }

# Add the install dir to the user PATH if missing.
$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if (-not ($userPath -split ';' | Where-Object { $_ -eq $InstallDir })) {
  Info "adding $InstallDir to your user PATH (restart the shell to pick it up)"
  $newPath = if ($userPath) { "$userPath;$InstallDir" } else { $InstallDir }
  [Environment]::SetEnvironmentVariable('Path', $newPath, 'User')
}

Info "installed: $exe"
Info "done. Nutanix deploy also needs Python 3 + 'pip install -r requirements.txt' (optional)."
