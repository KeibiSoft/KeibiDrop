@echo off
rem Alice – no-FUSE kd daemon on Windows (CMD).
rem Start relay first: run-local.bat in KeibiDrop-Relay\
set ROOT=%~dp0..\..
mkdir "%ROOT%\SaveAlice" 2>nul

set KD_NO_FUSE=1
if "%KD_RELAY%"=="" set KD_RELAY=http://localhost:54321
if "%KD_INBOUND_PORT%"=="" set KD_INBOUND_PORT=26001
if "%KD_OUTBOUND_PORT%"=="" set KD_OUTBOUND_PORT=26002
set KD_SAVE_PATH=%ROOT%\SaveAlice
set KD_LOG_FILE=%ROOT%\Log_Alice.txt
set KD_SOCKET=%TEMP%\kd-alice.sock

"%ROOT%\kd.exe" start
