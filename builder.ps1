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
        Write-Host "Build failed for $($target.GOOS)/$($target.GOARCH)" -ForegroundColor Red
        exit 1
    }
}

Write-Host "`nAll builds complete. Binaries are in '$outDir/'"
