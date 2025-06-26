package main

import (
	"archive/zip"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// fetchRemoteVersion retrieves the latest version from GitHub
func fetchRemoteVersion() (string, error) {
	debugLog("Checking for updates...")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get("https://raw.githubusercontent.com/teamcoltra/GeoStatsr/refs/heads/main/VERSION")
	if err != nil {
		return "", fmt.Errorf("failed to fetch remote version: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("failed to fetch remote version: HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read version response: %v", err)
	}

	version := strings.TrimSpace(string(data))
	debugLog("Remote version: %s, Current version: %s", version, currentVersion)
	return version, nil
}

// downloadAndExtractUpdate downloads the latest release and extracts it
func downloadAndExtractUpdate() (string, error) {
	debugLog("Downloading update from GitHub...")

	// Download the ZIP file
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get("https://github.com/teamcoltra/GeoStatsr/archive/refs/heads/main.zip")
	if err != nil {
		return "", fmt.Errorf("failed to download update: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("failed to download update: HTTP %d", resp.StatusCode)
	}

	// Create temporary file for the ZIP
	tmpFile, err := os.CreateTemp("", "geostatsr-update-*.zip")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %v", err)
	}
	defer tmpFile.Close()

	// Copy the download to the temp file
	_, err = io.Copy(tmpFile, resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to save download: %v", err)
	}

	// Create temporary directory for extraction
	tmpDir, err := os.MkdirTemp("", "geostatsr-extract-")
	if err != nil {
		return "", fmt.Errorf("failed to create extract directory: %v", err)
	}

	debugLog("Extracting update to: %s", tmpDir)

	// Extract the ZIP file
	err = unzipFile(tmpFile.Name(), tmpDir)
	if err != nil {
		return "", fmt.Errorf("failed to extract update: %v", err)
	}

	return tmpDir, nil
}

// unzipFile extracts a ZIP file to the specified destination
func unzipFile(src, dest string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer r.Close()

	// Ensure destination directory exists
	os.MkdirAll(dest, 0755)

	// Extract files
	for _, f := range r.File {
		err := extractFile(f, dest)
		if err != nil {
			return err
		}
	}

	return nil
}

// extractFile extracts a single file from a ZIP archive
func extractFile(f *zip.File, destPath string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	path := filepath.Join(destPath, f.Name)

	// Check for ZipSlip vulnerability
	if !strings.HasPrefix(path, filepath.Clean(destPath)+string(os.PathSeparator)) {
		return fmt.Errorf("invalid file path: %s", f.Name)
	}

	if f.FileInfo().IsDir() {
		os.MkdirAll(path, f.FileInfo().Mode())
		return nil
	}

	// Create directory if it doesn't exist
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}

	// Extract file
	outFile, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.FileInfo().Mode())
	if err != nil {
		return err
	}
	defer outFile.Close()

	_, err = io.Copy(outFile, rc)
	return err
}

// getNewBinaryPath returns the path to the new binary based on the platform
func getNewBinaryPath(extractDir string) string {
	basePath := filepath.Join(extractDir, "GeoStatsr-main", "dist")

	arch := runtime.GOARCH
	if runtime.GOOS == "darwin" {
		return filepath.Join(basePath, fmt.Sprintf("geostatsr-darwin-%s", arch))
	} else if runtime.GOOS == "windows" {
		return filepath.Join(basePath, fmt.Sprintf("geostatsr-windows-%s.exe", arch))
	} else {
		return filepath.Join(basePath, fmt.Sprintf("geostatsr-linux-%s", arch))
	}
}

// copyUpdatedFiles copies all non-binary files from the update to the config directory
func copyUpdatedFiles(extractDir string) error {
	sourcePath := filepath.Join(extractDir, "GeoStatsr-main")

	// Files/directories to copy to config directory
	itemsToCopy := []string{
		"static",
		"templates",
		"countries.json",
	}

	for _, item := range itemsToCopy {
		sourceItem := filepath.Join(sourcePath, item)
		destItem := filepath.Join(configDir, item)

		if _, err := os.Stat(sourceItem); os.IsNotExist(err) {
			debugLog("Skipping %s - not found in update", item)
			continue
		}

		debugLog("Copying %s to %s", sourceItem, destItem)
		err := copyFileOrDir(sourceItem, destItem)
		if err != nil {
			return fmt.Errorf("failed to copy %s: %v", item, err)
		}
	}

	return nil
}

// copyFileOrDir copies a file or directory recursively
func copyFileOrDir(src, dst string) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}

	if srcInfo.IsDir() {
		return copyDir(src, dst)
	}
	return copyFile(src, dst)
}

// copyFile copies a single file
func copyFile(src, dst string) error {
	sourceFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sourceFile.Close()

	// Create destination directory if it doesn't exist
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}

	destFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer destFile.Close()

	_, err = io.Copy(destFile, sourceFile)
	if err != nil {
		return err
	}

	// Copy file permissions
	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}
	return os.Chmod(dst, srcInfo.Mode())
}

// copyDir copies a directory recursively
func copyDir(src, dst string) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}

	err = os.MkdirAll(dst, srcInfo.Mode())
	if err != nil {
		return err
	}

	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())

		if entry.IsDir() {
			err = copyDir(srcPath, dstPath)
		} else {
			err = copyFile(srcPath, dstPath)
		}

		if err != nil {
			return err
		}
	}

	return nil
}

// replaceSelfWindows handles binary replacement on Windows using a batch script
func replaceSelfWindows(newBinaryPath, extractDir string) error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable path: %v", err)
	}

	// Create a persistent update script in the config directory (not temp)
	updateBatPath := filepath.Join(configDir, "geostatsr-update.bat")
	debugLog("Creating persistent update script: %s", updateBatPath)

	if err := createPersistentUpdateBat(updateBatPath, newBinaryPath, exePath, extractDir); err != nil {
		return fmt.Errorf("failed to create update script: %v", err)
	}

	debugLog("Created update script: %s", updateBatPath)
	debugLog("If update fails due to permissions, you can run this script as Administrator")

	// Prepare arguments for the batch script (restart args only)
	args := os.Args[1:]

	// Start the batch script
	cmd := exec.Command(updateBatPath, args...)
	err = cmd.Start()
	if err != nil {
		return fmt.Errorf("failed to start update script: %v", err)
	}

	debugLog("Update script started with PID %d", cmd.Process.Pid)
	return nil
}

// createPersistentUpdateBat creates a persistent update batch script in the config directory
func createPersistentUpdateBat(batPath, newBinaryPath, currentExePath, extractDir string) error {
	// Enhanced batch script with better error handling and persistence
	batchScript := fmt.Sprintf(`@echo off
setlocal enabledelayedexpansion

echo GeoStatsr Windows Update Script v0.5.0
echo ========================================
echo.
echo This script will update GeoStatsr to the latest version.
echo If you see permission errors, please run this script as Administrator.
echo.

REM Configuration
set "NEW_BINARY=%s"
set "CURRENT_EXE=%s"
set "EXTRACT_DIR=%s"
set "RESTART_ARGS=%%*"

echo Source binary: %%NEW_BINARY%%
echo Target binary: %%CURRENT_EXE%%
echo Extract directory: %%EXTRACT_DIR%%
echo Restart args: %%RESTART_ARGS%%
echo.

REM Wait a moment for the calling process to exit
echo [1/8] Waiting for calling process to exit...
timeout /t 3 /nobreak >nul

REM Try to stop the GeoStatsr service if it's running
echo [2/8] Checking for GeoStatsr service...
sc query "GeoStatsr" >nul 2>&1
if %%errorlevel%% equ 0 (
    echo Service found, attempting to stop...
    sc stop "GeoStatsr" >nul 2>&1
    timeout /t 5 /nobreak >nul
    echo Service stop command sent
) else (
    echo No service found or not running
)

REM Kill any running GeoStatsr processes
echo [3/8] Terminating any running GeoStatsr processes...
taskkill /F /IM "geostatsr.exe" >nul 2>&1
taskkill /F /IM "geostatsr-windows.exe" >nul 2>&1
taskkill /F /IM "main.exe" >nul 2>&1

REM Wait a bit more to ensure processes are fully terminated
timeout /t 2 /nobreak >nul

REM Check if the new binary exists
echo [4/8] Verifying new binary exists...
if not exist "%%NEW_BINARY%%" (
    echo ERROR: New binary not found at %%NEW_BINARY%%
    echo The download may have been corrupted or moved.
    pause
    exit /b 1
)

REM Backup the current binary
echo [5/8] Creating backup of current binary...
if exist "%%CURRENT_EXE%%" (
    move "%%CURRENT_EXE%%" "%%CURRENT_EXE%%.bak" >nul 2>&1
    if %%errorlevel%% neq 0 (
        echo ERROR: Failed to backup current binary
        echo This usually means the file is still in use or you need Administrator privileges.
        echo.
        echo Try the following:
        echo 1. Close all GeoStatsr instances
        echo 2. Run this script as Administrator
        echo 3. Or manually stop the GeoStatsr service first
        echo.
        pause
        exit /b 1
    )
    echo Backup created successfully
) else (
    echo No existing binary to backup
)

REM Move the new binary into place
echo [6/8] Installing new binary...
move "%%NEW_BINARY%%" "%%CURRENT_EXE%%" >nul 2>&1
if %%errorlevel%% neq 0 (
    echo ERROR: Failed to install new binary
    echo This usually means you need Administrator privileges.
    echo.
    echo Attempting to restore backup...
    if exist "%%CURRENT_EXE%%.bak" (
        move "%%CURRENT_EXE%%.bak" "%%CURRENT_EXE%%" >nul 2>&1
        if %%errorlevel%% equ 0 (
            echo Backup restored successfully
        ) else (
            echo ERROR: Failed to restore backup! 
            echo You may need to manually restore %%CURRENT_EXE%%.bak
        )
    )
    echo.
    echo To fix this issue:
    echo 1. Right-click this script and select "Run as administrator"
    echo 2. Or manually copy the files with elevated privileges
    echo.
    pause
    exit /b 1
)

REM Remove the backup if update was successful
if exist "%%CURRENT_EXE%%.bak" (
    del "%%CURRENT_EXE%%.bak" >nul 2>&1
    echo Old backup removed
)

echo [7/8] Binary update successful!

REM Try to restart the service first
echo [8/8] Attempting to restart GeoStatsr...
sc query "GeoStatsr" >nul 2>&1
if %%errorlevel%% equ 0 (
    echo Starting service...
    sc start "GeoStatsr" >nul 2>&1
    if %%errorlevel%% equ 0 (
        echo Service started successfully!
        goto cleanup
    ) else (
        echo Service failed to start, will start manually...
    )
) else (
    echo Service not installed, starting manually...
)

REM If service start failed or not installed, start manually
echo Starting GeoStatsr manually...
if "%%RESTART_ARGS%%"=="" (
    start "" "%%CURRENT_EXE%%"
) else (
    start "" "%%CURRENT_EXE%%" %%RESTART_ARGS%%
)

:cleanup
echo.
echo =========================================
echo Update completed successfully!
echo GeoStatsr has been updated and restarted.
echo =========================================

REM Clean up the extract directory
if exist "%%EXTRACT_DIR%%" (
    echo Cleaning up temporary files...
    rmdir /s /q "%%EXTRACT_DIR%%" >nul 2>&1
)

REM Clean up this script after a delay (if started automatically)
REM Note: If run manually by admin, script won't self-delete so user can see results
if "%%1"=="" (
    echo Cleaning up update script...
    timeout /t 2 /nobreak >nul
    del "%%~f0" >nul 2>&1
)

exit /b 0
`, newBinaryPath, currentExePath, extractDir)

	return os.WriteFile(batPath, []byte(batchScript), 0755)
}

// createTempUpdateBat creates a temporary update batch script
func createTempUpdateBat(path string) error {
	batchScript := `@echo off
setlocal enabledelayedexpansion

echo GeoStatsr Windows Update Script
echo ================================

REM Parameters: %1 = new binary path, %2 = current exe path, %3+ = restart args
set "NEW_BINARY=%~1"
set "CURRENT_EXE=%~2"
set "RESTART_ARGS=%~3"

REM Shift to get all remaining arguments for restart
:args_loop
shift
if "%~3"=="" goto args_done
set "RESTART_ARGS=!RESTART_ARGS! %~3"
goto args_loop
:args_done

echo New binary: %NEW_BINARY%
echo Current exe: %CURRENT_EXE%
echo Restart args: %RESTART_ARGS%

REM Wait a moment for the calling process to exit
echo Waiting for calling process to exit...
timeout /t 3 /nobreak >nul

REM Try to stop the GeoStatsr service if it's running
echo Attempting to stop GeoStatsr service...
sc query "GeoStatsr" >nul 2>&1
if !errorlevel! equ 0 (
    echo Service found, stopping it...
    sc stop "GeoStatsr" >nul 2>&1
    timeout /t 5 /nobreak >nul
) else (
    echo Service not found or not running
)

REM Kill any running GeoStatsr processes
echo Terminating any running GeoStatsr processes...
taskkill /F /IM "geostatsr.exe" >nul 2>&1
taskkill /F /IM "geostatsr-windows.exe" >nul 2>&1

REM Wait a bit more to ensure processes are fully terminated
timeout /t 2 /nobreak >nul

REM Backup the current binary
echo Creating backup of current binary...
if exist "%CURRENT_EXE%" (
    move "%CURRENT_EXE%" "%CURRENT_EXE%.bak" >nul 2>&1
    if !errorlevel! neq 0 (
        echo ERROR: Failed to backup current binary
        echo The file may still be in use. Please manually stop all GeoStatsr processes.
        pause
        exit /b 1
    )
)

REM Move the new binary into place
echo Installing new binary...
move "%NEW_BINARY%" "%CURRENT_EXE%" >nul 2>&1
if !errorlevel! neq 0 (
    echo ERROR: Failed to install new binary
    REM Try to restore the backup
    if exist "%CURRENT_EXE%.bak" (
        echo Restoring backup...
        move "%CURRENT_EXE%.bak" "%CURRENT_EXE%" >nul 2>&1
    )
    pause
    exit /b 1
)

REM Remove the backup if update was successful
if exist "%CURRENT_EXE%.bak" (
    del "%CURRENT_EXE%.bak" >nul 2>&1
)

echo Binary update successful!

REM Try to restart the service first
echo Attempting to restart GeoStatsr service...
sc query "GeoStatsr" >nul 2>&1
if !errorlevel! equ 0 (
    echo Starting service...
    sc start "GeoStatsr" >nul 2>&1
    if !errorlevel! equ 0 (
        echo Service started successfully!
        goto cleanup
    ) else (
        echo Service failed to start, will start manually...
    )
) else (
    echo Service not installed, starting manually...
)

REM If service start failed or not installed, start manually
echo Starting GeoStatsr manually...
if "%RESTART_ARGS%"=="" (
    start "" "%CURRENT_EXE%"
) else (
    start "" "%CURRENT_EXE%" %RESTART_ARGS%
)

:cleanup
echo Update complete!

REM Clean up this script after a delay
timeout /t 2 /nobreak >nul
del "%~f0" >nul 2>&1

exit /b 0
`

	return os.WriteFile(path, []byte(batchScript), 0755)
}

// replaceSelfUnix handles binary replacement on Unix-like systems
func replaceSelfUnix(newBinaryPath string) error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable path: %v", err)
	}

	// Make the new binary executable
	err = os.Chmod(newBinaryPath, 0755)
	if err != nil {
		return fmt.Errorf("failed to make new binary executable: %v", err)
	}

	// On Unix, we can replace the running binary
	err = os.Rename(newBinaryPath, exePath)
	if err != nil {
		return fmt.Errorf("failed to replace binary: %v", err)
	}

	return nil
}

// restartApp restarts the application with the same arguments
func restartApp() error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable path: %v", err)
	}

	debugLog("Restarting application: %s with args: %v", exePath, os.Args[1:])

	cmd := exec.Command(exePath, os.Args[1:]...)
	err = cmd.Start()
	if err != nil {
		return fmt.Errorf("failed to restart application: %v", err)
	}

	return nil
}

// performUpdate performs the complete update process
func performUpdate() error {
	debugLog("Starting update process...")

	// Download and extract the update
	extractDir, err := downloadAndExtractUpdate()
	if err != nil {
		return err
	}

	// Copy updated files to config directory
	err = copyUpdatedFiles(extractDir)
	if err != nil {
		// Clean up on copy failure
		os.RemoveAll(extractDir)
		return fmt.Errorf("failed to copy updated files: %v", err)
	}

	// Get the new binary path
	newBinaryPath := getNewBinaryPath(extractDir)
	if _, err := os.Stat(newBinaryPath); os.IsNotExist(err) {
		os.RemoveAll(extractDir)
		return fmt.Errorf("new binary not found at %s", newBinaryPath)
	}

	debugLog("Found new binary at: %s", newBinaryPath)

	// Replace the binary based on platform
	if runtime.GOOS == "windows" {
		err = replaceSelfWindows(newBinaryPath, extractDir)
		if err != nil {
			// Don't clean up extractDir on Windows - leave files for manual retry
			return fmt.Errorf("failed to replace binary on Windows: %v", err)
		}
		// On Windows, the batch script handles restart and cleanup, so we exit here
		debugLog("Update script started, exiting current process")
		os.Exit(0)
	} else {
		err = replaceSelfUnix(newBinaryPath)
		if err != nil {
			os.RemoveAll(extractDir) // Clean up on Unix failure
			return fmt.Errorf("failed to replace binary on Unix: %v", err)
		}

		// Clean up after successful Unix update
		os.RemoveAll(extractDir)

		// Restart the application
		err = restartApp()
		if err != nil {
			return fmt.Errorf("failed to restart application: %v", err)
		}

		debugLog("Application restarted, exiting current process")
		os.Exit(0)
	}

	return nil
}

// checkAndPerformUpdate checks for updates and performs them if available
func checkAndPerformUpdate(autoUpdate bool) {
	if !autoUpdate {
		debugLog("Auto-update disabled")
		return
	}

	debugLog("Checking for updates...")

	remoteVersion, err := fetchRemoteVersion()
	if err != nil {
		debugLog("Failed to check for updates: %v", err)
		return
	}

	// Simple string comparison - assumes semantic versioning
	if remoteVersion > currentVersion {
		if logger != nil {
			logger.Infof("Update available! Current: %s, Remote: %s", currentVersion, remoteVersion)
		} else {
			log.Printf("Update available! Current: %s, Remote: %s", currentVersion, remoteVersion)
		}

		err = performUpdate()
		if err != nil {
			if logger != nil {
				logger.Errorf("Update failed: %v", err)
			} else {
				log.Printf("Update failed: %v", err)
			}
		}
	} else {
		debugLog("No update available (remote: %s, current: %s)", remoteVersion, currentVersion)
	}
}
