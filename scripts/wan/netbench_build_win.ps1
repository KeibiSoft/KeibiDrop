# ABOUTME: Build netbench.exe on the Windows box from the synced source tarball.
# ABOUTME: Invoked via ssh with -File (never -Command: quoting eats $LASTEXITCODE).
param(
    [string]$Tgz = "C:/Windows/Temp/kd-netbench-src.tgz",
    [string]$Src = "C:/kd-bench/netbench-src"
)
$ErrorActionPreference = "Stop"
New-Item -ItemType Directory -Force -Path $Src | Out-Null
tar -xzf $Tgz -C $Src
Set-Location "$Src/scripts/wan/netbench"
$env:CGO_ENABLED = "0"
$env:GOFLAGS = "-buildvcs=false"
go build -o C:/kd-bench/netbench.exe .
if ($LASTEXITCODE -eq 0) {
    Write-Output "SG_BUILD_OK"
} else {
    Write-Output "SG_BUILD_FAIL exit=$LASTEXITCODE"
    exit 1
}
