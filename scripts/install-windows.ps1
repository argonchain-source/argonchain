# Installs the Argonmine node programs for the current user (no admin needed)
# in %LOCALAPPDATA%\Programs\Argonmine and adds that folder to the user PATH.
# Run from the unzipped release folder:
#   powershell -ExecutionPolicy Bypass -File install-windows.ps1     (-Uninstall to remove)
param([switch]$Uninstall)
$ErrorActionPreference = "Stop"
$src = $PSScriptRoot
$dest = Join-Path $env:LOCALAPPDATA "Programs\Argonmine"

if ($Uninstall) {
    Remove-Item -Recurse -Force $dest -ErrorAction SilentlyContinue
    # Shortcuts written by earlier versions of this script.
    Remove-Item -Recurse -Force (Join-Path ([Environment]::GetFolderPath("Programs")) "Argonmine") -ErrorAction SilentlyContinue
    Remove-Item (Join-Path ([Environment]::GetFolderPath("Desktop")) "Argonmine Miner.lnk") -ErrorAction SilentlyContinue
    $path = [Environment]::GetEnvironmentVariable("Path", "User")
    $new = ($path -split ";" | Where-Object { $_ -and $_ -ne $dest }) -join ";"
    [Environment]::SetEnvironmentVariable("Path", $new, "User")
    Write-Host "Argonmine removed (chain data and wallets in $env:LOCALAPPDATA\Argonmine were kept)."
    exit 0
}

New-Item -ItemType Directory -Force $dest | Out-Null
Get-ChildItem $src -Filter *.exe | ForEach-Object { Copy-Item $_.FullName $dest -Force }

$path = [Environment]::GetEnvironmentVariable("Path", "User")
if (($path -split ";") -notcontains $dest) {
    [Environment]::SetEnvironmentVariable("Path", "$path;$dest", "User")
}

Write-Host "Installed to $dest"
Write-Host "Open a new terminal, then start the node with:  argonmined"
