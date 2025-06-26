@echo off
setlocal enabledelayedexpansion

REM GeoStatsr Windows Installation Script
echo GeoStatsr Windows Installation
echo ==============================

REM Check if running as administrator
net session >nul 2>&1
if !errorlevel! neq 0 (
    echo Error: This script must be run as Administrator
    echo Right-click on Command Prompt and select "Run as Administrator"
    pause
    exit /b 1
)

REM Get the current directory (where the script is located)
set "SOURCE_DIR=%~dp0"
if "%SOURCE_DIR:~-1%"=="\" set "SOURCE_DIR=%SOURCE_DIR:~0,-1%"
set "INSTALL_DIR=C:\Program Files\GeoStatsr"

REM Detect system architecture
set "ARCHITECTURE=amd64"
if "%PROCESSOR_ARCHITECTURE%"=="ARM64" set "ARCHITECTURE=arm64"

REM Determine input binary
set "INPUT_BINARY=%SOURCE_DIR%\dist\geostatsr-windows-%ARCHITECTURE%.exe"
if not exist "%INPUT_BINARY%" (
    echo ERROR: Expected binary not found: %INPUT_BINARY%
    echo Make sure dist\geostatsr-windows-%ARCHITECTURE%.exe exists
    pause
    exit /b 1
)

echo Detected architecture: %ARCHITECTURE%
echo Installing from: %INPUT_BINARY%
echo Target install dir: %INSTALL_DIR%

REM Create install directory if it doesn't exist
if not exist "%INSTALL_DIR%" mkdir "%INSTALL_DIR%"

REM Copy everything from current folder to install dir
xcopy "%SOURCE_DIR%\*" "%INSTALL_DIR%\" /E /I /Y /Q

REM Move correct binary into place
move /Y "%INPUT_BINARY%" "%INSTALL_DIR%\geostatsr.exe"

REM Delete the dist directory entirely
rmdir /S /Q "%INSTALL_DIR%\dist"

REM Remove other platform-specific files if they exist
del /Q "%INSTALL_DIR%\geostatsr-linux" >nul 2>&1
del /Q "%INSTALL_DIR%\geostatsr-mac" >nul 2>&1
del /Q "%INSTALL_DIR%\linux-setup.sh" >nul 2>&1
del /Q "%INSTALL_DIR%\mac-setup.sh" >nul 2>&1

REM Change to install directory and install service
cd /d "%INSTALL_DIR%"
echo Installing GeoStatsr service...
geostatsr.exe -s install

if !errorlevel! equ 0 (
    echo Service installed successfully!
    echo Starting service...
    geostatsr.exe -s start

    if !errorlevel! equ 0 (
        echo.
        echo Installation complete!
        echo GeoStatsr is now running as a Windows service
        echo Web interface: http://localhost:62826
        echo.
        echo Service commands:
        echo   Start:   sc start GeoStatsr
        echo   Stop:    sc stop GeoStatsr
        echo   Status:  sc query GeoStatsr
        echo   Restart: sc stop GeoStatsr && sc start GeoStatsr
        echo.
        echo Or use Windows Services Manager (services.msc)
    ) else (
        echo Service installed but failed to start
        echo You can start it manually using: sc start GeoStatsr
    )
) else (
    echo Failed to install service
    exit /b 1
)

echo.
pause