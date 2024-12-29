# Ensure the script stops on error
$ErrorActionPreference = "Stop"

# Check if dce is installed
if (-not (Get-Command "dce" -ErrorAction SilentlyContinue)) {
    Write-Error "dce is not installed or not in your PATH."
    exit 1
}

# Pass all arguments to dce
try {
    & dce @args
} catch {
    Write-Error "An error occurred while running dce."
    exit 1
}
