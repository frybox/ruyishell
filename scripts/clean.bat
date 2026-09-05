@echo off
setlocal

rem Project root = parent of the directory containing this script (scripts\..)
rem Used as absolute path everywhere so the script is cwd-independent.
set "ROOT=%~dp0.."

rem Remove build output dirs (mirrors: make clean)
if exist "%ROOT%\bin" rmdir /s /q "%ROOT%\bin"
if exist "%ROOT%\dist" rmdir /s /q "%ROOT%\dist"

echo [clean] done: removed bin/ and dist/
