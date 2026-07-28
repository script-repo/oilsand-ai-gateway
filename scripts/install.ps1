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
  $stage = Join-Path $tmp 'stage'
  Info "downloading $Asset ($Version)"
  Invoke-WebRequest -Headers @{ 'User-Agent' = 'oilsand-install' } -Uri $Url -OutFile $zip

  Info "extracting to staging area"
  New-Item -ItemType Directory -Path $stage -Force | Out-Null
  Expand-Archive -Path $zip -DestinationPath $stage -Force

  # GoReleaser may nest files one level deep or flat — normalize to $stage root.
  $nested = Get-ChildItem -LiteralPath $stage -Directory -ErrorAction SilentlyContinue |
    Where-Object { Test-Path (Join-Path $_.FullName "$BinName.exe") } |
    Select-Object -First 1
  $srcRoot = if ($nested) { $nested.FullName } else { $stage }

  $newExe = Join-Path $srcRoot "$BinName.exe"
  if (-not (Test-Path -LiteralPath $newExe)) {
    Fail "binary not found in archive: expected $BinName.exe under $srcRoot"
  }

  Info "installing to $InstallDir"
  New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null

  # Self-update safe copy: never Remove-Item the whole install dir while
  # oilsand-tui.exe may be the currently running process (Windows locks it and
  # the Update section used to fail with deploy/delete failed rc=1).
  Get-ChildItem -LiteralPath $srcRoot -Force | ForEach-Object {
    $dest = Join-Path $InstallDir $_.Name
    if ($_.PSIsContainer) {
      if (Test-Path -LiteralPath $dest) {
        # Keep an existing venv; refresh scripts/ and other trees.
        if ($_.Name -eq 'venv') {
          Info "keeping existing venv (will refresh pip deps below)"
          return
        }
        Remove-Item -LiteralPath $dest -Recurse -Force -ErrorAction SilentlyContinue
      }
      Copy-Item -LiteralPath $_.FullName -Destination $dest -Recurse -Force
      return
    }
    if ($_.Name -eq "$BinName.exe") {
      # Replace running binary via rename dance when the target is locked.
      $bak = Join-Path $InstallDir "$BinName.exe.old"
      if (Test-Path -LiteralPath $dest) {
        Remove-Item -LiteralPath $bak -Force -ErrorAction SilentlyContinue
        try {
          Move-Item -LiteralPath $dest -Destination $bak -Force
        } catch {
          # Still locked under another name — try direct overwrite (fails if locked).
          Info "could not retire previous binary ($($_.Exception.Message)); trying overwrite"
        }
      }
      try {
        Copy-Item -LiteralPath $_.FullName -Destination $dest -Force
      } catch {
        Fail "could not install $BinName.exe while it is running. Close oilsand-tui and re-run: irm https://raw.githubusercontent.com/script-repo/oilsand-ai-gateway/main/scripts/install.ps1 | iex"
      }
      # Best-effort cleanup of previous binary; ignore if still mapped.
      Remove-Item -LiteralPath $bak -Force -ErrorAction SilentlyContinue
      return
    }
    Copy-Item -LiteralPath $_.FullName -Destination $dest -Force
  }
} finally {
  Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}

$exe = Join-Path $InstallDir "$BinName.exe"
if (-not (Test-Path -LiteralPath $exe)) { Fail "binary not found after extraction: $exe" }

# Add the install dir to the user PATH if missing.
$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if (-not ($userPath -split ';' | Where-Object { $_ -eq $InstallDir })) {
  Info "adding $InstallDir to your user PATH (restart the shell to pick it up)"
  $newPath = if ($userPath) { "$userPath;$InstallDir" } else { $InstallDir }
  [Environment]::SetEnvironmentVariable('Path', $newPath, 'User')
}

Info "installed: $exe"

# ---- interactive feature dependencies --------------------------------------
# Nutanix deploy shells out to Python (requests + paramiko); the Console/Agents
# features shell out to the OpenSSH client. Best-effort setup so the install has
# everything. Set $env:OILSAND_SKIP_DEPS = '1' to skip this section.
function Have($name) { [bool](Get-Command $name -ErrorAction SilentlyContinue) }

if ($env:OILSAND_SKIP_DEPS -eq '1') {
  Info 'skipping dependency setup (OILSAND_SKIP_DEPS=1)'
} else {
  # OpenSSH client (Console / Agents).
  if (Have ssh) {
    Info 'openssh client: ok'
  } else {
    Info 'openssh client missing; attempting to add the Windows OpenSSH client capability'
    try {
      Add-WindowsCapability -Online -Name 'OpenSSH.Client~~~~0.0.1.0' -ErrorAction Stop | Out-Null
      Info 'openssh client installed'
    } catch {
      if (Have winget) {
        try { winget install -e --id Microsoft.OpenSSH.Beta --accept-source-agreements --accept-package-agreements | Out-Null } catch {}
      }
      if (-not (Have ssh)) {
        Info 'WARNING: could not install the OpenSSH client (needed for Console/Agents).'
        Info '  Enable it via Settings > System > Optional features > OpenSSH Client, or run as admin:'
        Info '  Add-WindowsCapability -Online -Name OpenSSH.Client~~~~0.0.1.0'
      }
    }
  }

  # Resolve a Python launcher (install via winget if absent).
  $py = $null
  foreach ($cand in @('py', 'python', 'python3')) { if (Have $cand) { $py = $cand; break } }
  if (-not $py) {
    Info 'Python 3 missing; attempting install via winget'
    if (Have winget) {
      try { winget install -e --id Python.Python.3.12 --accept-source-agreements --accept-package-agreements | Out-Null } catch {}
    }
    foreach ($cand in @('py', 'python', 'python3')) { if (Have $cand) { $py = $cand; break } }
  }

  $req  = Join-Path $InstallDir 'requirements.txt'
  $venv = Join-Path $InstallDir 'venv'
  if ($py -and (Test-Path $req)) {
    Info "setting up Python venv for Nutanix deploy ($venv)"
    try {
      & $py -m venv $venv
      $vpy = Join-Path $venv 'Scripts\python.exe'
      & $vpy -m pip install --quiet --upgrade pip | Out-Null
      & $vpy -m pip install --quiet -r $req
      if ($LASTEXITCODE -eq 0) {
        Info "python deps installed (requests, paramiko); the TUI uses $venv automatically"
      } else {
        Info "WARNING: pip install failed; run:  `"$vpy`" -m pip install -r `"$req`""
      }
    } catch {
      Info "WARNING: could not set up the Python venv ($_)."
      Info "  Install Python 3, then:  $py -m pip install -r `"$req`""
    }
  } elseif (-not $py) {
    Info 'WARNING: Python 3 not found (needed for Nutanix deploy). Install from https://www.python.org/downloads/'
  }
}

Info 'done.'
