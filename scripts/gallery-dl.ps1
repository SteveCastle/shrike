# Ensure the script stops on error
$ErrorActionPreference = "Stop"

# Check if gallery-dl is installed
if (-not (Get-Command "gallery-dl" -ErrorAction SilentlyContinue)) {
    Write-Error "gallery-dl is not installed or not in your PATH."
    exit 1
}

# Pass all arguments to gallery-dl
try {
    & gallery-dl @args
} catch {
    Write-Error "An error occurred while running gallery-dl."
    exit 1
}
