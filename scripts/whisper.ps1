# Ensure the script stops on error
$ErrorActionPreference = "Stop"

# Check if dce is installed
if (-not (Get-Command "faster-whisper-xxl" -ErrorAction SilentlyContinue)) {
    Write-Error "faster-whisper-xxl is not installed or not in your PATH."
    exit 1
}

# Pass all arguments to faster-whisper-xxl
try {
    & faster-whisper-xxl @args
} catch {
    Write-Error "An error occurred while running faster-whisper-xxl."
    exit 1
}
