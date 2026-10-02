# One KeibiDrop peer for a cross-network interop scenario, run inside ONE logon
# session on the Windows fleet host. wan_pass_win.ps1 explains why: unix-socket
# dials from other ssh sessions fail intermittently on this box, in-session
# dials work. The orchestrator (xnet_interop.sh) reads marker lines from this
# script's stdout and drops handoff files into -Dir: peer.fp, go-pull, done.
param(
  [string]$Exe,
  [string]$Dir,
  [string]$Name,
  [string]$Role,          # create | join
  [int]$InPort = 26461,
  [int]$OutPort = 26462,
  [int]$SizeMiB = 50
)
$ErrorActionPreference = "Continue"
function Say([string]$s) { Write-Host $s; [Console]::Out.Flush() }
function WaitFile([string]$p, [int]$sec) {
  $d = (Get-Date).AddSeconds($sec)
  while (-not (Test-Path $p) -and (Get-Date) -lt $d) { Start-Sleep -Milliseconds 500 }
  return (Test-Path $p)
}
function Kd() { return (& $Exe @args 2>&1 | Out-String) }
# A handoff file can be seen before its writer closed it; read until it is whole.
function ReadHandoff([string]$p) {
  for ($i = 0; $i -lt 60; $i++) {
    try {
      $v = (Get-Content $p -First 1 -ErrorAction Stop)
      if ($v -and $v.Trim().Length -gt 0) { return $v.Trim() }
    } catch { }
    Start-Sleep -Milliseconds 300
  }
  return ""
}

# Only a daemon started from THIS binary is stopped. Never a blanket kill.
Get-Process kd -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $Exe } | Stop-Process -Force -ErrorAction SilentlyContinue
Remove-Item -Recurse -Force $Dir -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force -Path "$Dir/save" | Out-Null

$env:KD_SOCKET = "$Dir/kd.sock"
$env:KD_SAVE_PATH = "$Dir/save"
$env:KD_NO_FUSE = "1"
$env:KD_INCOGNITO = "1"
$env:KD_INBOUND_PORT = "$InPort"
$env:KD_OUTBOUND_PORT = "$OutPort"
$env:KD_LOG_FILE = "$Dir/kd.log"
$p = Start-Process -FilePath $Exe -ArgumentList "start" -WorkingDirectory $Dir `
     -RedirectStandardOutput "$Dir/stdout.txt" -RedirectStandardError "$Dir/stderr.txt" -PassThru -WindowStyle Hidden

function Bail([string]$why) {
  Say ("FAIL " + $why)
  Get-Content "$Dir/stdout.txt" -Tail 8 -ErrorAction SilentlyContinue | ForEach-Object { Say ("OUT " + $_) }
  if (-not $p.HasExited) {
    & $Exe stop 2>&1 | Out-Null
    Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue
  }
  Say "DONE"
  exit 1
}

$fp = $null
for ($i = 0; $i -lt 60; $i++) {
  Start-Sleep -Seconds 1
  $out = Kd show fingerprint
  if ($out -match '"ok":true') { $fp = (ConvertFrom-Json $out).data.fingerprint; break }
}
if (-not $fp) { Bail "daemon never answered show fingerprint" }
Say "FP $fp"

$file = "$Dir/from-$Name.bin"
$rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()
$buf = New-Object byte[] 1048576
$fs = [IO.File]::Create($file)
for ($i = 0; $i -lt $SizeMiB; $i++) { $rng.GetBytes($buf); $fs.Write($buf, 0, $buf.Length) }
$fs.Close()
Say ("MYSHA " + (Get-FileHash $file -Algorithm SHA256).Hash.ToLower())

if (-not (WaitFile "$Dir/peer.fp" 900)) { Bail "peer.fp never arrived" }
$peer = ReadHandoff "$Dir/peer.fp"
$r = Kd register $peer
if ($r -notmatch '"ok":true') { Bail ("register: " + $r.Trim()) }
Say "REGISTERED"

$t0 = Get-Date
if ($Role -eq "create") { Say "CREATING" } else { Say "JOINING" }
$c = Start-Process -FilePath $Exe -ArgumentList @($Role, "--timeout=150") -WorkingDirectory $Dir `
     -RedirectStandardOutput "$Dir/$Role.out" -RedirectStandardError "$Dir/$Role.err" -PassThru -WindowStyle Hidden
$w = Kd wait-connected --timeout=170
$secs = [math]::Round(((Get-Date) - $t0).TotalSeconds)
if ($w -match '"ok":true') { Say "CONNECTED $secs" } else {
  Say ("CONNECT FAIL after $secs s: " + $w.Trim())
  Get-Content "$Dir/$Role.out" -Tail 5 -ErrorAction SilentlyContinue | ForEach-Object { Say ("OUT " + $_) }
}
$c.WaitForExit(5000) | Out-Null

$a = Kd add $file
if ($a -match '"ok":true') { Say "ADDED" } else { Say ("ADD FAIL " + $a.Trim()) }

if (WaitFile "$Dir/go-pull" 600) {
  $remote = ReadHandoff "$Dir/go-pull"
  $got = "$Dir/got-$remote"
  $tp = Get-Date
  $pl = Kd pull $remote $got --timeout=600
  $ps = [math]::Round(((Get-Date) - $tp).TotalSeconds)
  if (Test-Path $got) { Say ("GOTSHA " + (Get-FileHash $got -Algorithm SHA256).Hash.ToLower() + " " + $ps) }
  else { Say ("PULL FAIL after $ps s: " + $pl.Trim()) }
} else { Say "FAIL go-pull never arrived" }

if (-not (WaitFile "$Dir/done" 900)) { Say "no done file; stopping anyway" }
# Stop only the daemon this script started. A stale copy of this script must
# never reach the socket of a newer run.
if (-not $p.HasExited) {
  & $Exe stop 2>&1 | Out-Null
  Start-Sleep -Seconds 1
  Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue
}
Get-Content "$Dir/kd.log" -ErrorAction SilentlyContinue |
  Select-String -Pattern 'Cipher negotiated|falling back to bridge|Direct P2P|QUIC|tiebreak|level=ERROR' |
  Select-Object -First 12 | ForEach-Object {
    $l = ($_.Line -replace '^.*?msg=', 'msg=')
    Say ("LOG " + $l.Substring(0, [Math]::Min(170, $l.Length)))
  }
Say "DONE"
exit 0
