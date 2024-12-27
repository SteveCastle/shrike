# Ensure the script stops on error
$ErrorActionPreference = "Stop"

# Check if ls is available (either as an alias or external command)
if (-not (Get-Command "ls" -ErrorAction SilentlyContinue)) {
    Write-Error "'ls' is not available. Ensure it is accessible in your current shell."
    exit 1
}

# Pass all arguments to ls
try {
    & ls @args
} catch {
    Write-Error "An error occurred while running 'ls'."
    exit 1
}
