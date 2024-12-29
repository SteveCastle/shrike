# Check if path argument is provided
if ($args.Count -eq 0) {
    Write-Host "Please provide a directory path as an argument."
    Write-Host "Usage: .\script.ps1 <directory_path>"
    exit 1
}

# Get the directory path from first argument
$directoryPath = $args[0]

# Verify the directory exists
if (-not (Test-Path -Path $directoryPath -PathType Container)) {
    Write-Host "Directory does not exist: $directoryPath"
    exit 1
}

# Check if ffmpeg is available
try {
    $null = Get-Command ffmpeg
}
catch {
    Write-Host "FFmpeg is not found in PATH. Please install FFmpeg and make sure it's in your system PATH."
    exit 1
}

# Get all .ts files in the directory
$tsFiles = Get-ChildItem -Path $directoryPath -Filter "*.ts"

if ($tsFiles.Count -eq 0) {
    Write-Host "No .ts files found in directory: $directoryPath"
    exit 1
}

# Counter for progress tracking
$current = 0
$total = $tsFiles.Count

foreach ($file in $tsFiles) {
    $current++
    $outputFile = Join-Path $directoryPath ($file.BaseName + ".mp4")
    
    Write-Host "[$current/$total] Converting: $($file.Name)"
    
    try {
        # FFmpeg command using NVIDIA NVENC for H.265 encoding
        ffmpeg -i $file.FullName `
            -c:v hevc_nvenc `
            -preset p4 `
            -rc vbr `
            -cq 23 `
            -b:v 0 `
            -maxrate 15M `
            -bufsize 30M `
            -c:a aac `
            -b:a 192k `
            $outputFile `
            -y

        if ($LASTEXITCODE -eq 0) {
            Write-Host "Successfully converted: $($file.Name)" -ForegroundColor Green
        }
        else {
            Write-Host "Error converting: $($file.Name)" -ForegroundColor Red
        }
    }
    catch {
        Write-Host "Exception while converting $($file.Name): $_" -ForegroundColor Red
    }
}

Write-Host "Conversion complete! Processed $total files."