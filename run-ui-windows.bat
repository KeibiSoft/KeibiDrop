@echo off
REM ============================================================================
REM  KeibiDrop UI launcher (Windows) — Pass the Salt demo
REM
REM  Enforces a CONSISTENT mount point. TO_MOUNT_PATH is the single source of
REM  truth: it feeds (1) the engine's FUSE mount, (2) the "Open Folder" button,
REM  and (3) the UI's mount-path display. Set it here once and they can't
REM  disagree. Launch the UI ONLY through this script (or a shortcut to it).
REM
REM  Mount shows up as drive K:. Use a different free letter (M:, P:, X:, Z:)
REM  or a folder path (e.g. %USERPROFILE%\KeibiDrop\Mount) if you prefer.
REM ============================================================================

set "TO_MOUNT_PATH=K:"
set "TO_SAVE_PATH=%USERPROFILE%\KeibiDrop\Received"

REM Same-Wi-Fi LAN mode (phone <-> laptop, laptop <-> Mac). Remove this line
REM for an internet/relay demo.
set "KEIBIDROP_LOCAL=1"

REM WinFsp on PATH (cgofuse also finds it via the registry; harmless belt-and-braces)
set "PATH=C:\Program Files (x86)\WinFsp\bin;%PATH%"

cd /d "%~dp0"
echo Launching KeibiDrop UI  -  mount = %TO_MOUNT_PATH%   save = %TO_SAVE_PATH%
start "KeibiDrop" "%~dp0rust\target\release\keibidrop-rust.exe"
