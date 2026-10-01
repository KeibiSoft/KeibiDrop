# Build kd.exe on the Windows fleet host.
# Copied to the host and invoked with -File, never inlined into an ssh command
# line. Nesting a bash double-quoted string inside ssh inside powershell
# -Command eats the backslash escapes: $LASTEXITCODE arrived as empty and the
# guard rendered as `if ( -ne 0)`. fleet/remote.ps1 uses -File for this reason.
param([string]$Src = "C:/kd-bench/src")

$ErrorActionPreference = "Stop"

New-Item -ItemType Directory -Force -Path $Src | Out-Null
Set-Location $Src
tar -xzf C:/Windows/Temp/kd-wan-src.tgz

# The synced dir is not a git checkout, so buildvcs stamping fails.
$env:GOFLAGS = "-buildvcs=false"
# WinFsp lives under "C:\Program Files (x86)". The space breaks cgo with
# "invalid flag: Files". The 8.3 short path is the fix and must be set BEFORE
# the build.
$env:CGO_LDFLAGS = "-LC:/PROGRA~2/WinFsp/lib"

Write-Host ">>> go build ./cmd/kd/"
go build -o kd.exe ./cmd/kd/
if ($LASTEXITCODE -ne 0) {
    Write-Host "BUILD FAILED, exit $LASTEXITCODE"
    exit 1
}

$f = Get-Item kd.exe
Write-Host ("kd.exe  {0:N0} bytes  {1}" -f $f.Length, $f.LastWriteTime)
exit 0
