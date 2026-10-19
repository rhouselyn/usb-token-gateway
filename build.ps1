<#
  build.ps1 — 无需任何个人路径：脚本自己从所在位置推断项目根目录。

  用法（在项目根目录）：
      powershell -ExecutionPolicy Bypass -File .\build.ps1            # 开发版 + 正式版
      powershell -ExecutionPolicy Bypass -File .\build.ps1 -Release   # 只出正式版

  产物：
      go\usbgw.exe            控制台开发版（调试用，有 stdout）
      usb\usb-bridge.exe      正式版（无黑框，托盘）——拷贝 usb\ 文件夹到 U 盘即用
#>
[CmdletBinding()]
param(
    [switch]$Release,
    [switch]$Dev,
    [string]$GoBin = ""
)

$ErrorActionPreference = "Stop"
$Root = Split-Path -Parent $MyInvocation.MyCommand.Path
$GoDir = Join-Path $Root "go"
$UsbDir = Join-Path $Root "usb"

function Resolve-Go {
    if ($GoBin) { return $GoBin }
    $cmd = Get-Command go.exe -ErrorAction SilentlyContinue
    if ($cmd) { return $cmd.Source }
    foreach ($p in @(
            (Join-Path $env:ProgramFiles "Go\bin\go.exe"),
            (Join-Path ${env:ProgramFiles(x86)} "Go\bin\go.exe"),
            (Join-Path $env:LOCALAPPDATA "Programs\Go\bin\go.exe"),
            "C:\Go\bin\go.exe")) {
        if ($p -and (Test-Path $p)) { return $p }
    }
    throw "找不到 go.exe，请安装 Go 或用 -GoBin 指定路径"
}

$go = Resolve-Go
Write-Host "[build] go      = $go"
Write-Host "[build] project = $Root"

if (-not $Release -or $Dev) {
    Write-Host "[build] 开发版 go\usbgw.exe ..."
    Push-Location $GoDir
    & $go build -o "usbgw.exe" .
    $devOk = ($LASTEXITCODE -eq 0)
    Pop-Location
    if (-not $devOk) { throw "开发版构建失败" }
}

if ($Release -or (-not $Dev)) {
    Write-Host "[build] 正式版 usb\usb-bridge.exe ..."
    $tmp = Join-Path $env:TEMP "usb-bridge-build.exe"
    Push-Location $GoDir
    & $go build -trimpath -ldflags "-H windowsgui -s -w" -o $tmp .
    $relOk = ($LASTEXITCODE -eq 0)
    Pop-Location
    if (-not $relOk) { throw "正式版构建失败" }
    New-Item -ItemType Directory -Force -Path $UsbDir | Out-Null
    $dst = Join-Path $UsbDir "usb-bridge.exe"
    try {
        Copy-Item $tmp $dst -Force
    } catch {
        Write-Warning "覆盖 $dst 失败（多半是托盘里的 USB Bridge 还在运行）。"
        Write-Warning "请右键托盘图标 → 退出，然后重新运行本脚本。"
        throw
    }
    Write-Host "[build] 完成：usb\usb-bridge.exe（整个 usb\ 文件夹拷进 U 盘即可用）"
}

Write-Host "[build] OK"
