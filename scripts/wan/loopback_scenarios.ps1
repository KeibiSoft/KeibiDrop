# Loopback scenarios on one Windows host: two kd daemons, production relay and bridge.
#   powershell -File loopback_scenarios.ps1 -Exe C:\path\kd.exe -Work C:\path\work [-Mount Z:]
# One PASS, FAIL or SKIP line per check; exit 1 if anything failed. -Mount nofuse skips the mount checks.
param([string]$Exe, [string]$Work, [string]$Mount = "Z:")
$ErrorActionPreference = "Continue"
$PA = 26471; $PB = 26491
$fails = 0
function ok($m) { Write-Output "PASS $m" }
function bad($m) { Write-Output "FAIL $m"; $script:fails++ }
function skip($m) { Write-Output "SKIP $m" }
function kdc($p) { $env:KD_SOCKET = "$Work\$p\kd.sock"; $out = & $Exe @args 2>&1 | Out-String; return $out.Trim() }
function fpof($json) { try { return (ConvertFrom-Json $json).data.fingerprint } catch { return "" } }
function fp_of($p) { for ($i = 0; $i -lt 60; $i++) { $f = fpof (kdc $p show fingerprint); if ($f) { return $f }; Start-Sleep 1 }; return "" }
function sha($path) { (Get-FileHash -Algorithm SHA256 $path).Hash }
function connected($p, $t) { (kdc $p wait-connected "--timeout=$t") -match '"connected":true' }
$procs = @()
function start_kd($p, $port, $fuse) {
  $d = "$Work\$p"; New-Item -ItemType Directory -Force -Path "$d\save", "$d\config" | Out-Null
  Remove-Item "$d\kd.sock" -ErrorAction SilentlyContinue
  $env:KEIBIDROP_CONFIG_DIR = "$d\config"; $env:KD_SOCKET = "$d\kd.sock"; $env:KD_SAVE_PATH = "$d\save"
  $env:KD_MOUNT_PATH = $Mount; $env:KD_NO_FUSE = $(if ($fuse) { "0" } else { "1" }); $env:KD_INCOGNITO = "1"
  $env:KD_INBOUND_PORT = "$port"; $env:KD_OUTBOUND_PORT = "$($port + 1)"; $env:KD_LOG_FILE = "$d\kd.log"
  $script:procs += Start-Process -FilePath $Exe -ArgumentList "start" -WorkingDirectory $d -WindowStyle Hidden -PassThru -RedirectStandardOutput "$d\stdout" -RedirectStandardError "$d\stderr"
}
function bg($p, $verb, $outfile) { $env:KD_SOCKET = "$Work\$p\kd.sock"; return Start-Process -FilePath $Exe -ArgumentList @($verb, "--timeout=120") -WindowStyle Hidden -PassThru -RedirectStandardOutput $outfile -RedirectStandardError "$outfile.err" }
function rnd($path, $n) { $b = New-Object byte[] $n; (New-Object Random).NextBytes($b); [IO.File]::WriteAllBytes($path, $b) }
$t0 = Get-Date
function ts($m) { Write-Output ("[{0}s] {1}" -f [int]((Get-Date) - $t0).TotalSeconds, $m) }

try {
  # A shares its save dir on start, with a trash folder, a recycle bin and a bitmap sidecar planted in it.
  New-Item -ItemType Directory -Force -Path "$Work\A\config", "$Work\A\save\.Trashes\0", ("$Work\A\save\" + '$RECYCLE.BIN\S-1') | Out-Null
  Set-Content -Path "$Work\A\config\config.toml" -Value "scan_shared_on_start = true`nrescan_shared_seconds = 5"
  rnd "$Work\A\save\from-a.bin" 300000
  rnd "$Work\A\save\.Trashes\0\trashed.bin" 1000
  rnd ("$Work\A\save\" + '$RECYCLE.BIN\S-1\binned.bin') 1000
  rnd "$Work\A\save\part.bin.kdbitmap" 16
  start_kd A $PA $false; start_kd B $PB ($Mount -ne "nofuse")
  $fpA = fp_of A; $fpB = fp_of B
  if (-not $fpA -or -not $fpB) { bad "daemons up (A=$fpA B=$fpB)"; Get-Content "$Work\A\stdout","$Work\B\stdout" -ErrorAction SilentlyContinue | Select-Object -Last 4; throw "no daemons" }
  ok "daemons up"
  kdc A register $fpB | Out-Null; kdc B register $fpA | Out-Null

  # 1. Cancel, then connect again.
  $j1 = bg B join "$Work\join1.out"; Start-Sleep 6; kdc B disconnect | Out-Null; $j1.WaitForExit(20000) | Out-Null
  $o = Get-Content "$Work\join1.out" -Raw -ErrorAction SilentlyContinue
  if ($o -match "cancelled") { ok "cancel ends the wait" } else { bad "cancel: $o" }
  $fpB2 = fp_of B; if ($fpB2 -eq $fpB) { ok "code unchanged after cancel" } else { bad "code rotated after cancel"; kdc A register $fpB2 | Out-Null }
  $j2 = bg B join "$Work\join2.out"; Start-Sleep 2; $c1 = bg A create "$Work\create1.out"
  if ((connected A 90) -and (connected B 90)) { ok ("connect after cancel ({0}s)" -f [int]((Get-Date) - $t0).TotalSeconds) } else { bad "connect after cancel" }
  $j2.WaitForExit(5000) | Out-Null; $c1.WaitForExit(5000) | Out-Null
  ts ("B " + ((kdc B status) -replace '.*("connection_mode":"[a-z]+").*', '$1'))

  # 2. Shared-folder scan hygiene.
  Start-Sleep 3; $L = kdc B list
  if ($L -match "from-a.bin") { ok "scan shares from-a.bin" } else { bad "scan: from-a.bin missing: $($L.Substring(0, [Math]::Min(200, $L.Length)))" }
  if ($L -match "Trashes|kdbitmap|RECYCLE") { bad "scan leaks trash, recycle bin or sidecar" } else { ok "scan hides the trash folder, the recycle bin and the sidecar" }

  # 3. Transfer both ways.
  kdc B pull from-a.bin "$Work\B\got-a.bin" "--timeout=120" | Out-Null
  if ((Test-Path "$Work\B\got-a.bin") -and ((sha "$Work\B\got-a.bin") -eq (sha "$Work\A\save\from-a.bin"))) { ok "pull A->B" } else { bad "pull A->B" }
  rnd "$Work\B\from-b.bin" 300000; kdc B add "$Work\B\from-b.bin" | Out-Null; Start-Sleep 2
  kdc A pull from-b.bin "$Work\A\got-b.bin" "--timeout=120" | Out-Null
  if ((Test-Path "$Work\A\got-b.bin") -and ((sha "$Work\A\got-b.bin") -eq (sha "$Work\B\from-b.bin"))) { ok "pull B->A" } else { bad "pull B->A" }

  # 4. WinFsp mount on B: trash folders refused, a plain folder allowed, a move into an existing trash folder refused.
  if ($Mount -ne "nofuse" -and ((kdc B wait-mount "--timeout=30") -match '"ready":true')) {
    $M = $Mount
    $r = New-Item -ItemType Directory -Path "$M\.Trashes" -ErrorAction SilentlyContinue; if ($r) { bad "mkdir .Trashes succeeded on the mount" } else { ok "mkdir .Trashes refused" }
    $r = New-Item -ItemType Directory -Path ("$M\" + '$RECYCLE.BIN') -ErrorAction SilentlyContinue; if ($r) { bad 'mkdir $RECYCLE.BIN succeeded on the mount' } else { ok 'mkdir $RECYCLE.BIN refused' }
    $r = New-Item -ItemType Directory -Path "$M\plain" -ErrorAction SilentlyContinue; if ($r) { ok "mkdir plain allowed"; Remove-Item "$M\plain" -ErrorAction SilentlyContinue } else { bad "mkdir plain refused" }
    New-Item -ItemType Directory -Force -Path "$Work\B\save\.Trashes\0" | Out-Null
    Move-Item "$M\from-a.bin" "$M\.Trashes\0\from-a.bin" -ErrorAction SilentlyContinue
    if (Test-Path "$M\from-a.bin") { ok "move into an existing trash folder refused, file still in place" } else { bad "move into an existing trash folder succeeded" }
  } else { skip "mount checks (no mount)" }

  # 5. The relay poll while waiting is not an error line; the lane.
  if (Select-String -Path "$Work\B\kd.log" -Pattern 'level=ERROR msg="Failed to fetch"' -Quiet) { bad "404 poll logged at ERROR" } else { ok "404 poll is quiet" }
  if (Select-String -Path "$Work\B\kd.log" -Pattern "QUIC control lane verified" -Quiet) { ok "QUIC lane up" } else { skip "QUIC lane not verified on B" }

  # 6. Always-on: A stays up, B disconnects, both register again, B connects again.
  kdc B disconnect | Out-Null; Start-Sleep 2
  $fpA = fp_of A; $fpB = fp_of B; kdc A register $fpB | Out-Null; kdc B register $fpA | Out-Null
  $j3 = bg B join "$Work\join3.out"; Start-Sleep 1; $c2 = bg A create "$Work\create2.out"
  if ((connected A 90) -and (connected B 90)) { ok "second connect to the same peer" } else { bad "second connect" }
  $j3.WaitForExit(5000) | Out-Null; $c2.WaitForExit(5000) | Out-Null; Start-Sleep 2
  if ((kdc B list) -match "from-a.bin") { ok "files listed again after reconnect" } else { bad "files missing after reconnect" }
} finally {
  kdc A disconnect | Out-Null; kdc B disconnect | Out-Null; kdc A stop | Out-Null; kdc B stop | Out-Null; Start-Sleep 1
  foreach ($p in $procs) { if ($p -and -not $p.HasExited) { Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue } }
  Get-Process kd -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $Exe } | Stop-Process -Force -ErrorAction SilentlyContinue
}
Write-Output "done: $fails failure(s); logs in $Work\A\kd.log and $Work\B\kd.log"
exit $(if ($fails -eq 0) { 0 } else { 1 })
