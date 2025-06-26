#!/bin/bash

# GeoStatsr Mac Installation Script
echo "GeoStatsr Mac Installation"
echo "=========================="

# Ensure running as root
if [ "$EUID" -ne 0 ]; then
    echo "Error: This script must be run as root (use sudo)"
    echo "Usage: sudo ./mac-setup.sh"
    exit 1
fi

SOURCE_DIR="$(cd "$(dirname "$0")"; pwd)"
INSTALL_DIR="/usr/local/geostatsr"

# Detect architecture
ARCH=$(uname -m)
if [ "$ARCH" = "arm64" ]; then
    BINARY_NAME="geostatsr-darwin-arm64"
else
    BINARY_NAME="geostatsr-darwin-amd64"
fi

INPUT_BINARY="$SOURCE_DIR/dist/$BINARY_NAME"
TARGET_BINARY="$INSTALL_DIR/geostatsr"

if [ ! -f "$INPUT_BINARY" ]; then
    echo "ERROR: Expected binary not found: $INPUT_BINARY"
    echo "Make sure 'dist/$BINARY_NAME' exists"
    exit 1
fi

echo "Detected architecture: $ARCH"
echo "Installing from: $INPUT_BINARY"
echo "Installing to: $INSTALL_DIR"

# Create install dir
mkdir -p "$INSTALL_DIR"

# Copy everything
cp -r "$SOURCE_DIR"/* "$INSTALL_DIR/"

# Rename binary
mv -f "$INPUT_BINARY" "$TARGET_BINARY"
chmod +x "$TARGET_BINARY"

# Remove dist and irrelevant files
rm -rf "$INSTALL_DIR/dist"
rm -f "$INSTALL_DIR/geostatsr-linux"
rm -f "$INSTALL_DIR/geostatsr-windows.exe"
rm -f "$INSTALL_DIR/linux-setup.sh"
rm -f "$INSTALL_DIR/windows-setup.bat"

# Install service
echo "Installing service..."
cd "$INSTALL_DIR"
"$TARGET_BINARY" -s install

if [ $? -eq 0 ]; then
    echo "Service installed successfully!"
    echo "Starting service..."
    "$TARGET_BINARY" -s start

    if [ $? -eq 0 ]; then
        echo ""
        echo "Installation complete!"
        echo "GeoStatsr is now running as a system service"
        echo "Web interface: http://localhost:62826"
        echo ""
        echo "Service commands:"
        echo "  Start:   sudo launchctl load /Library/LaunchDaemons/com.geostatsr.service.plist"
        echo "  Stop:    sudo launchctl unload /Library/LaunchDaemons/com.geostatsr.service.plist"
        echo "  Status:  sudo launchctl list | grep geostatsr"
        echo "  Restart: sudo launchctl unload /Library/LaunchDaemons/com.geostatsr.service.plist && sudo launchctl load /Library/LaunchDaemons/com.geostatsr.service.plist"
    else
        echo "Service installed but failed to start"
        echo "You can start it manually with: sudo launchctl load /Library/LaunchDaemons/com.geostatsr.service.plist"
    fi
else
    echo "Failed to install service"
    exit 1
fi
