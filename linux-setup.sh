#!/bin/bash

# GeoStatsr Linux Installation Script
echo "GeoStatsr Linux Installation"
echo "============================"

# Ensure script is run as root
if [ "$EUID" -ne 0 ]; then
    echo "Error: This script must be run as root (use sudo)"
    echo "Usage: sudo ./linux-setup.sh"
    exit 1
fi

SOURCE_DIR="$(cd "$(dirname "$0")"; pwd)"
INSTALL_DIR="/opt/geostatsr"

# Detect architecture
ARCH=$(uname -m)
if [ "$ARCH" = "aarch64" ]; then
    BINARY_NAME="geostatsr-linux-arm64"
else
    BINARY_NAME="geostatsr-linux-amd64"
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

# Create install directory
mkdir -p "$INSTALL_DIR"

# Copy all files from source
cp -r "$SOURCE_DIR"/* "$INSTALL_DIR/"

# Move and rename the correct binary
mv -f "$INPUT_BINARY" "$TARGET_BINARY"
chmod +x "$TARGET_BINARY"

# Remove dist and platform-specific extras
rm -rf "$INSTALL_DIR/dist"
rm -f "$INSTALL_DIR/geostatsr-mac"
rm -f "$INSTALL_DIR/geostatsr-windows.exe"
rm -f "$INSTALL_DIR/geostatsr.exe"
rm -f "$INSTALL_DIR/mac-setup.sh"
rm -f "$INSTALL_DIR/windows-setup.bat"

# Install the systemd service
echo "Installing service..."
cd "$INSTALL_DIR"
./geostatsr -s install

if [ $? -eq 0 ]; then
    echo "Service installed successfully!"
    echo "Starting service..."
    ./geostatsr -s start

    if [ $? -eq 0 ]; then
        echo ""
        echo "Installation complete!"
        echo "GeoStatsr is now running as a system service"
        echo "Web interface: http://localhost:62826"
        echo ""
        echo "Service commands:"
        echo "  Start:   sudo systemctl start GeoStatsr"
        echo "  Stop:    sudo systemctl stop GeoStatsr"
        echo "  Status:  sudo systemctl status GeoStatsr"
        echo "  Restart: sudo systemctl restart GeoStatsr"
    else
        echo "Service installed but failed to start"
        echo "You can start it manually with: sudo systemctl start GeoStatsr"
    fi
else
    echo "Failed to install service"
    exit 1
fi
