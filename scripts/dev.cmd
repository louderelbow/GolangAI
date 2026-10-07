@echo off
rem ============================================================
rem  DeepTalk dev environment launcher (wrapper for dev.ps1)
rem
rem  Why this wrapper exists:
rem    dev.ps1 must be run by a PowerShell that decodes UTF-8
rem    correctly. PowerShell 7 does it by default; Windows
rem    PowerShell 5.1 needs the file to carry a UTF-8 BOM.
rem    This wrapper picks pwsh when available, falls back to
rem    powershell, and switches the console to UTF-8 so the
rem    Chinese output does not turn into garbage.
rem
rem  NOTE: keep this file pure ASCII. cmd.exe reads .cmd files
rem  using the system OEM codepage (GBK on zh-CN), so any
rem  non-ASCII byte here gets mangled and cmd tries to run it.
rem
rem  Usage:
rem    scripts\dev.cmd -Check     health check only
rem    scripts\dev.cmd            start MCP + backend + frontend
rem    scripts\dev.cmd -Stop      stop them
rem ============================================================

rem 65001 = UTF-8. Without this the Chinese messages are garbage
rem when the console is in GBK.
chcp 65001 >nul 2>nul

setlocal
set "DEEPTALK_PS=powershell"

rem Prefer PowerShell 7: cleaner UTF-8 handling, supports && etc.
where pwsh >nul 2>nul
if %ERRORLEVEL% EQU 0 set "DEEPTALK_PS=pwsh"

"%DEEPTALK_PS%" -NoLogo -NoProfile -ExecutionPolicy Bypass -File "%~dp0dev.ps1" %*
set "EXITCODE=%ERRORLEVEL%"

endlocal & exit /b %EXITCODE%
