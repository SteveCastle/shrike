# Ensure the script stops on error
$ErrorActionPreference = "Stop"

# Check if gallery-dl is installed
if (-not (Get-Command "oc" -ErrorAction SilentlyContinue)) {
    Write-Error "oc is not installed or not in your PATH."
    exit 1
}

# Pass all arguments to oc
try {
    & oc @args
} catch {
    Write-Error "An error occurred while running oc."
    exit 1
}
