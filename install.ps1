<#
.SYNOPSIS
    Build and install the `loop` CLI (goop) from source.
.DESCRIPTION
    Compiles loop.exe with `go build` and copies it to ~/.goop/bin, adding that
    directory to your user PATH if needed. Run from the repo root.
.PARAMETER InstallDir
    Where to put the binary. Defaults to ~/.goop/bin.
#>
param(
    [string]$InstallDir = (Join-Path $HOME ".goop" "bin")
)

$ErrorActionPreference = "Stop"
$repoRoot = Split-Path -Parent $MyInvocation.MyCommand.Path

$ext = if ($IsWindows -or -not $IsLinux -and -not $IsMacOS) { ".exe" } else { "" }
$binName = "loop$ext"
$out = Join-Path $repoRoot $binName

Write-Host "Building $binName ..."
Push-Location $repoRoot
try {
    & go build -o $binName .
    if ($LASTEXITCODE -ne 0) { throw "go build failed" }
} finally {
    Pop-Location
}

New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
$dest = Join-Path $InstallDir $binName
Copy-Item $out $dest -Force
Write-Host "Installed to $dest"

$userPath = [Environment]::GetEnvironmentVariable("PATH", "User")
if ($userPath -notlike "*$InstallDir*") {
    [Environment]::SetEnvironmentVariable("PATH", "$userPath;$InstallDir", "User")
    Write-Host "Added $InstallDir to user PATH (restart your shell to pick it up)"
}

& $dest whoami
