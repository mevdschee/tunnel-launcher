@echo off
REM Build tunnel-launcher.exe for Windows with embedded icon and no console window.
REM Requires: go, fyne CLI (go install fyne.io/fyne/v2/cmd/fyne@latest)

echo Building tunnel-launcher.exe ...
fyne package -os windows -icon icon.png -name tunnel-launcher
if %ERRORLEVEL% NEQ 0 (
    echo.
    echo ERROR: fyne package failed.
    echo Make sure fyne CLI is installed:
    echo   go install fyne.io/fyne/v2/cmd/fyne@latest
    exit /b 1
)

for %%F in (tunnel-launcher.exe) do echo Done: %%~nxF  (%%~zF bytes)
