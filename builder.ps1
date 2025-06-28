# Set the output directory and binary name
$binaryName = "geostatsr"
$outDir = "dist"
New-Item -ItemType Directory -Force -Path $outDir | Out-Null

# Define build targets
$targets = @(
    @{ GOOS = "windows"; GOARCH = "amd64"; EXT = ".exe"; CGO = "1" },
    @{ GOOS = "windows"; GOARCH = "arm64"; EXT = ".exe"; CGO = "1" },
    @{ GOOS = "linux";   GOARCH = "amd64"; EXT = "";    CGO = "0" },
    @{ GOOS = "linux";   GOARCH = "arm64"; EXT = "";    CGO = "0" },
    @{ GOOS = "darwin";  GOARCH = "amd64"; EXT = "";    CGO = "0" },
    @{ GOOS = "darwin";  GOARCH = "arm64"; EXT = "";    CGO = "0" }
)

# Save original environment values
$originalGOOS = $env:GOOS
$originalGOARCH = $env:GOARCH
$originalCGO = $env:CGO_ENABLED

# Build loop
foreach ($target in $targets) {
    $env:GOOS = $target.GOOS
    $env:GOARCH = $target.GOARCH
    $env:CGO_ENABLED = $target.CGO

    $outputFile = "$binaryName-$($target.GOOS)-$($target.GOARCH)$($target.EXT)"
    $outputPath = Join-Path $outDir $outputFile

    Write-Host "Building $outputFile (CGO_ENABLED=$($target.CGO))..."
    go build -o $outputPath

    if ($LASTEXITCODE -ne 0) {
        Write-Host "❌ Build failed for $($target.GOOS)/$($target.GOARCH)" -ForegroundColor Red

        # Restore original env vars before exiting
        if ($null -ne $originalGOOS) { $env:GOOS = $originalGOOS } else { Remove-Item Env:GOOS -ErrorAction SilentlyContinue }
        if ($null -ne $originalGOARCH) { $env:GOARCH = $originalGOARCH } else { Remove-Item Env:GOARCH -ErrorAction SilentlyContinue }
        if ($null -ne $originalCGO) { $env:CGO_ENABLED = $originalCGO } else { Remove-Item Env:CGO_ENABLED -ErrorAction SilentlyContinue }

        exit 1
    }
}

# Restore original env vars
if ($null -ne $originalGOOS) { $env:GOOS = $originalGOOS } else { Remove-Item Env:GOOS -ErrorAction SilentlyContinue }
if ($null -ne $originalGOARCH) { $env:GOARCH = $originalGOARCH } else { Remove-Item Env:GOARCH -ErrorAction SilentlyContinue }
if ($null -ne $originalCGO) { $env:CGO_ENABLED = $originalCGO } else { Remove-Item Env:CGO_ENABLED -ErrorAction SilentlyContinue }

Write-Host "`n✅ All builds complete. Binaries are in '$outDir/'"
