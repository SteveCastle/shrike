param (
    [string]$Directory = ".",
    [int]$Depth = 0
)

Get-ChildItem -Path $Directory -Recurse -Depth $Depth -File |
    Where-Object { $_.Extension -match "\.jpg|\.jpeg|\.png|\.gif|\.bmp|\.tiff|\.mp4|\.mkv|\.avi|\.mov|\.wmv|\.flv|\.webm" } |
    Select-Object FullName
