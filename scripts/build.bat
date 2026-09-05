@echo off
setlocal

rem Project root = parent of the directory containing this script (scripts\..)
rem Used as absolute path everywhere so the script is cwd-independent.
set "ROOT=%~dp0.."

rem Version: reuse git describe, fall back to dev
for /f "tokens=*" %%v in ('git -C "%ROOT%" describe --tags --always --dirty 2^>nul') do set VERSION=%%v
if not defined VERSION set VERSION=dev

rem Match Makefile: static build, no C compiler
set CGO_ENABLED=0
set GOFLAGS=-trimpath
set LDFLAGS=-X main.version=%VERSION%

if not exist "%ROOT%\bin" mkdir "%ROOT%\bin"
go -C "%ROOT%" build %GOFLAGS% -ldflags "%LDFLAGS%" -o "%ROOT%\bin\rysh.exe" ./cmd/rysh

if errorlevel 1 (
    echo [build] failed
    exit /b 1
)

echo [build] done: %ROOT%\bin\rysh.exe  (version=%VERSION%)
