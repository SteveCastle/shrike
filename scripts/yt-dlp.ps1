# Ensure the script stops on error
$ErrorActionPreference = "Stop"

# Check if dce is installed
if (-not (Get-Command "yt-dlp" -ErrorAction SilentlyContinue)) {
    Write-Error "yt-dlp is not installed or not in your PATH."
    exit 1
}

# Pass all arguments to yt-dlp
try {
    & yt-dlp @args
} catch {
    Write-Error "An error occurred while running yt-dlp."
    exit 1
}
