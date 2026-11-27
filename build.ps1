param(
    [string]$Version = "dev",
    [ValidateSet("amd64", "arm64")]
    [string]$Arch = "amd64"
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $root

New-Item -ItemType Directory -Force -Path dist | Out-Null
$env:GOOS = "windows"
$env:GOARCH = $Arch
$env:CGO_ENABLED = "0"

$out = if ($Arch -eq "amd64") { "dist/uefimgr.exe" } else { "dist/uefimgr-$Arch.exe" }
go build -trimpath -ldflags "-s -w -H=windowsgui -X main.version=$Version" -o $out ./cmd/uefimgr
if ($LASTEXITCODE -ne 0) { throw "build failed" }

Write-Host "Built $out (version $Version). Double-click it; Windows asks for administrator rights."
