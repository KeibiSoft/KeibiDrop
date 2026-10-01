# Probe: can the Windows kd.exe run a daemon and answer the CLI?
# The daemon socket is documented as a "Unix socket path (default /tmp/kd.sock)"
# and there is no _windows.go variant anywhere. Go supports AF_UNIX on Windows
# 10 1803+, so a Windows path may work, but /tmp/kd.sock does not exist here.
# This answers it in isolation, before any cross-host orchestration depends on it.
param(
  [string]$Src  = "C:/kd-bench/src",
  [string]$Sock = "C:/kd-bench/kd-wan-b.sock",
  [string]$Save = "C:/kd-bench/WanSaveB"
)
$ErrorActionPreference = "Continue"
Set-Location $Src

# Clean only what this probe owns.
Get-Process kd -ErrorAction SilentlyContinue |
  Where-Object { $_.Path -eq (Join-Path $Src "kd.exe") } |
  Stop-Process -Force -ErrorAction SilentlyContinue
Remove-Item $Sock -Force -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force -Path $Save | Out-Null

$env:KD_SOCKET    = $Sock
$env:KD_SAVE_PATH = $Save
$env:KD_NO_FUSE   = "1"
$env:KD_INCOGNITO = "1"
$env:KD_LOG_FILE  = "C:/kd-bench/kd-wan-b.log"

Write-Host ">>> starting daemon"
$p = Start-Process -FilePath (Join-Path $Src "kd.exe") -ArgumentList "start" `
     -PassThru -WindowStyle Hidden `
     -RedirectStandardOutput "C:/kd-bench/kd-wan-b.out" `
     -RedirectStandardError  "C:/kd-bench/kd-wan-b.err"
Write-Host ("pid: {0}" -f $p.Id)

for ($i = 0; $i -lt 20; $i++) {
    Start-Sleep -Milliseconds 500
    if (Test-Path $Sock) { break }
}
Write-Host ("socket exists after wait: {0}" -f (Test-Path $Sock))

Write-Host ">>> kd show fingerprint"
& (Join-Path $Src "kd.exe") show fingerprint
Write-Host ("cli exit: {0}" -f $LASTEXITCODE)

Write-Host ">>> daemon stderr (last 15)"
if (Test-Path "C:/kd-bench/kd-wan-b.err") { Get-Content "C:/kd-bench/kd-wan-b.err" -Tail 15 }
Write-Host ">>> daemon log (last 15)"
if (Test-Path $env:KD_LOG_FILE) { Get-Content $env:KD_LOG_FILE -Tail 15 }

Write-Host ">>> stopping probe daemon"
Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue
