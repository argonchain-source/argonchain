# Cross-compiles the node operator's programs for every supported platform and
# writes zips plus SHA256SUMS to .\release\. Pure Go, no cgo, so this runs
# anywhere Go does. The browser app is the product for users; these archives
# are for people who run a node (docs/NODE.md, "Download").
#
#   powershell -ExecutionPolicy Bypass -File scripts\release.ps1 -Version 1.1.0
param(
    [string]$Version = "1.1.0",
    [string]$Go = "go"
)
$ErrorActionPreference = "Stop"
Set-Location (Join-Path $PSScriptRoot "..")
$out = "release"
New-Item -ItemType Directory -Force $out | Out-Null

$targets = @(
    @{os="windows"; arch="amd64"},
    @{os="windows"; arch="arm64"},
    @{os="linux";   arch="amd64"},
    @{os="linux";   arch="arm64"},
    @{os="darwin";  arch="amd64"},
    @{os="darwin";  arch="arm64"}
)
$cmds = @("argonmined", "argonmine-wallet", "argonmine-explorer", "argonmine-miner", "genesis")

# Fetch every module up front so cross-compiles never hit the network, and
# build the archiver for this machine (forward-slash paths, programs marked
# executable for Linux and macOS, fixed timestamps).
& $Go mod download 2>&1 | Out-Null
$mkzip = Join-Path $out "mkzip.exe"
& $Go build -o $mkzip ./scripts/mkzip
if ($LASTEXITCODE -ne 0) { throw "build failed: scripts/mkzip" }

foreach ($t in $targets) {
    $name = "argonmine-$Version-$($t.os)-$($t.arch)"
    $dir = Join-Path $out $name
    if (Test-Path $dir) { Remove-Item -Recurse -Force $dir }
    New-Item -ItemType Directory -Force $dir | Out-Null
    $env:GOOS = $t.os; $env:GOARCH = $t.arch; $env:CGO_ENABLED = "0"
    foreach ($c in $cmds) {
        $exe = $c; if ($t.os -eq "windows") { $exe = "$c.exe" }
        $buildOut = & $Go build -trimpath -ldflags "-s -w -X main.version=$Version" -o (Join-Path $dir $exe) "./cmd/$c" 2>&1
        if ($LASTEXITCODE -ne 0) { Write-Host ($buildOut | Out-String); throw "build failed: $c for $($t.os)/$($t.arch)" }
    }
    Copy-Item SECURITY.md $dir
    Copy-Item scripts\README-node.txt (Join-Path $dir "README.txt")
    New-Item -ItemType Directory -Force (Join-Path $dir "docs") | Out-Null
    Copy-Item docs\NODE.md, docs\SPEC.md (Join-Path $dir "docs")
    if ($t.os -eq "windows") { Copy-Item scripts\install-windows.ps1 $dir } else { Copy-Item scripts\install.sh $dir }
    $zip = Join-Path $out "$name.zip"
    if (Test-Path $zip) { Remove-Item $zip }
    & $mkzip $zip $dir
    if ($LASTEXITCODE -ne 0) { throw "archive failed: $zip" }
    Remove-Item -Recurse -Force $dir
    Write-Host "built $zip"
}
Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED -ErrorAction SilentlyContinue
Remove-Item $mkzip

$sums = Get-ChildItem (Join-Path $out "*.zip") | ForEach-Object {
    "$((Get-FileHash $_.FullName -Algorithm SHA256).Hash.ToLower())  $($_.Name)"
}
# Unix line endings, so `sha256sum -c` and `shasum -c` read it on Linux and macOS.
[IO.File]::WriteAllText((Join-Path (Resolve-Path $out) "SHA256SUMS"), (($sums -join "`n") + "`n"))
Write-Host "wrote $out\SHA256SUMS"
