@echo off
rem Alice – FUSE kd daemon on Windows (CMD). Requires WinFSP installed.
rem Mounts peer files to drive letter Z: (or set KD_MOUNT_PATH=Z:).
rem Start relay first: run-local.bat in KeibiDrop-Relay\
set ROOT=%~dp0..\..
mkdir "%ROOT%\SaveAlice" 2>nul

if "%KD_RELAY%"=="" set KD_RELAY=http://localhost:54321
if "%KD_INBOUND_PORT%"=="" set KD_INBOUND_PORT=26001
if "%KD_OUTBOUND_PORT%"=="" set KD_OUTBOUND_PORT=26002
if "%KD_MOUNT_PATH%"=="" set KD_MOUNT_PATH=Z:
set KD_SAVE_PATH=%ROOT%\SaveAlice
set KD_LOG_FILE=%ROOT%\Log_Alice_FUSE.txt
set KD_SOCKET=%TEMP%\kd-alice.sock

"%ROOT%\kd.exe" start
