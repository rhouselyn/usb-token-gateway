<#
  publish.ps1 — 一条命令把仓库发到 GitHub（含 SEO 元数据与 Release）

  前置：装好 GitHub CLI 并登录一次
      winget install GitHub.cli
      gh auth login -h github.com -p https -w

  用法（在项目根目录）：
      powershell -ExecutionPolicy Bypass -File .\publish.ps1
            # 建仓（public）+ 提交 + 推送 + 设置描述/topics/主页

      powershell -ExecutionPolicy Bypass -File .\publish.ps1 -Version v1.0.0
            # 上面全部 + 构建 usb\usb-bridge.exe 并作为 Release 附件发布

      powershell -ExecutionPolicy Bypass -File .\publish.ps1 -Private
            # 改成私有仓库（注意：私有仓的 Release 外人也拿不到）

  说明：脚本自己从 gh 读你的账号名与数字 ID，本仓库的提交身份默认设成
        GitHub 的 noreply 邮箱（不公开真实邮箱），不会动你的全局 git 配置。
#>
[CmdletBinding()]
param(
    [string]$Repo = "usb-token-gateway",
    [string]$Version = "",
    [string]$Description = "Portable local LLM API gateway on a USB stick: OpenAI- & Anthropic-compatible endpoints, multi-channel failover, protocol translation, encrypted key storage. 便携式本地 LLM 网关 / U盘 Token 中转站。",
    [string]$CommitMessage = "feat: USB Token Gateway —— U盘即插即用的本地 LLM token 中转站",
    [switch]$Private,
    [switch]$SkipDocs
)

$ErrorActionPreference = "Stop"
$Root = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $Root

# 20 个 topic（GitHub 上限就是 20），兼顾英文检索与分类
$Topics = @(
    "llm", "llm-gateway", "llm-proxy", "ai-gateway", "api-gateway",
    "openai", "openai-api", "anthropic", "claude", "claude-api",
    "api-key-management", "secrets-management", "reverse-proxy", "sse",
    "usb", "portable", "windows", "golang", "self-hosted", "developer-tools"
)

function Fail($msg) { Write-Host "[x] $msg" -ForegroundColor Red; exit 1 }
function Step($msg) { Write-Host "[publish] $msg" -ForegroundColor Cyan }

# PS 5.1 下 $ErrorActionPreference=Stop 时，原生命令 stderr 一旦重定向就会被
# 包装成终止错误；这里临时降级为 Continue 再重定向丢弃，用退出码判断结果。
function Test-Quiet([scriptblock]$sb) {
    $prev = $ErrorActionPreference
    $ErrorActionPreference = "Continue"
    try { & $sb 2>$null | Out-Null } finally { $ErrorActionPreference = $prev }
    return ($LASTEXITCODE -eq 0)
}

# ---------------------------------------------------------------- 0) 检查 gh
if (-not (Get-Command gh -ErrorAction SilentlyContinue)) {
    Fail "未找到 gh。先执行：winget install GitHub.cli（装完重开终端）"
}
if (-not (Test-Quiet { gh api user })) {
    Fail "gh 认证不可用。可执行：gh auth login -h github.com -p https -w；或设置 GH_TOKEN 环境变量"
}

$Owner = (gh api user --jq .login).Trim()
$Id    = (gh api user --jq .id).Trim()
if (-not $Owner) { Fail "无法从 gh 取到账号名" }
Step "账号：$Owner (id=$Id)  目标仓库：$Owner/$Repo"

# ---------------------------------------------------------------- 1) 提交身份
# 优先用 noreply 邮箱，避免把真实邮箱写进公开历史；已显式设过则尊重原设置
$localEmail = (git config --local user.email) 2>$null
if (-not $localEmail) {
    git config --local user.name  $Owner
    git config --local user.email "$Id+$Owner@users.noreply.github.com"
    Step "本仓库提交身份设为 $Id+$Owner@users.noreply.github.com（noreply，不影响全局配置）"
}

# ---------------------------------------------------------------- 2) 首次提交
if (-not (Test-Quiet { git rev-parse --verify HEAD })) {
    git add -A
    git commit -m $CommitMessage | Out-Null
    Step "已创建首次提交"
} else {
    $dirty = git status --porcelain
    if ($dirty) {
        git add -A
        git commit -m $CommitMessage | Out-Null
        Step "已提交新的改动"
    } else {
        Step "工作区干净，无需提交"
    }
}

# ---------------------------------------------------------------- 3) 建仓/推送
$visibility = if ($Private) { "--private" } else { "--public" }
if (Test-Quiet { gh repo view "$Owner/$Repo" }) {
    Step "仓库已存在，直接推送"
    if (-not (Test-Quiet { git remote get-url origin })) { git remote add origin "https://github.com/$Owner/$Repo.git" }
    git branch -M main
    git push -u origin main
} else {
    Step "创建仓库并推送"
    gh repo create "$Owner/$Repo" $visibility --source . --remote origin --push --description $Description
}
if ($LASTEXITCODE -ne 0) { Fail "推送失败" }

# ---------------------------------------------------------------- 4) SEO 元数据
if (-not $SkipDocs) {
    Step "设置仓库描述 / topics / 主页"
    gh repo edit "$Owner/$Repo" --description $Description --homepage "https://github.com/$Owner/$Repo/releases" --add-topic ($Topics -join ",") | Out-Null
    if ($LASTEXITCODE -ne 0) { Write-Host "[!] topics/描述设置失败，可在网页 Settings 手动补" -ForegroundColor Yellow }
}

# ---------------------------------------------------------------- 5) Release
if ($Version) {
    if ($Version -notmatch '^v') { $Version = "v$Version" }
    Step "构建正式版 exe"
    & (Join-Path $Root "build.ps1") -Release

    # ---- 内置 Codex CLI（/v1/responses 客户端）打进 usb\bin\ ----
    $binDir = Join-Path $Root "usb\bin"
    New-Item -ItemType Directory -Force -Path $binDir | Out-Null
    $codexExe = Join-Path $binDir "codex.exe"
    if (Test-Path $codexExe) {
        Step "codex.exe 已存在（usb\bin），跳过下载"
    } else {
        Step "下载 Codex CLI（openai/codex latest · windows-x64）"
        $ProgressPreference = "SilentlyContinue"
        [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
        $got = $false
        foreach ($i in 1..3) {
            try {
                $rel = Invoke-RestMethod -Uri "https://api.github.com/repos/openai/codex/releases/latest" -TimeoutSec 60
                $asset = $rel.assets |
                    Where-Object { $_.name -like "*x86_64-pc-windows-msvc*" -and $_.name -like "*.zip" } |
                    Select-Object -First 1
                if (-not $asset) { throw "release 资产里没找到 windows-x64 zip" }
                $zip = Join-Path $env:TEMP "codex-cli-win.zip"
                Invoke-WebRequest -Uri $asset.browser_download_url -OutFile $zip -TimeoutSec 900
                $unz = Join-Path $env:TEMP "codex-cli-win"
                if (Test-Path $unz) { Remove-Item $unz -Recurse -Force }
                Expand-Archive -Path $zip -DestinationPath $unz -Force
                $inner = Get-ChildItem $unz -Recurse -Filter "*.exe" | Select-Object -First 1
                if (-not $inner) { throw "zip 里没有 exe" }
                Copy-Item $inner.FullName $codexExe -Force
                $got = $true
                break
            } catch {
                Write-Host "[!] 第 $i 次下载失败：$($_.Exception.Message)" -ForegroundColor Yellow
                if ($i -lt 3) { Start-Sleep -Seconds 45 }
            }
        }
        if (-not $got) {
            Write-Host "[!] codex.exe 下载失败 —— 本版 Release 将不带 Codex，可稍后重跑 publish.ps1" -ForegroundColor Yellow
        }
    }

    # GitHub 服务端强制 ASCII 附件名（中文名会被改成 default.txt）
    # → 先拷成 ASCII 名上传，创建完再用 PATCH 打中文 label
    $guideSrc = Join-Path $Root "usb\使用说明.txt"
    $batSrc   = Join-Path $Root "usb\启动 Codex.bat"
    $guide    = Join-Path $Root "usb\usage-guide.txt"
    $launcher = Join-Path $Root "usb\launch-codex.bat"
    if (Test-Path $guideSrc) { Copy-Item $guideSrc $guide -Force }
    if (Test-Path $batSrc)   { Copy-Item $batSrc $launcher -Force }

    $assets = @()
    foreach ($f in @("usb\usb-bridge.exe", "usb\bin\codex.exe", "usb\usage-guide.txt", "usb\launch-codex.bat")) {
        $p = Join-Path $Root $f
        if (Test-Path $p) { $assets += $p } else { Write-Host "[!] 缺少 $f（跳过该附件）" -ForegroundColor Yellow }
    }
    if (-not $assets) { Fail "没有可作为附件的文件" }
    Step "发布 $Version（附件：$($assets.Count) 个）"
    gh release create $Version @assets --title "$Version — U盘即插即用的本地 LLM token 中转站" `
        --notes "双击即用：首次设一次主密码，之后静默启动、不弹浏览器。`n`n- OpenAI 兼容端点：``/v1/chat/completions``、``/v1/models``、``/v1/embeddings```n- OpenAI Responses 端点：``/v1/responses``（内置 Codex CLI 即插即用）`n- Anthropic 原生端点：``/v1/messages``（含 SSE 流式）`n- 多渠道轮换 / 熔断 / 429 冷却`n- 面板勾选「Codex」Key + 模型名 → 实时写进 U 盘 Codex 配置`n- 上游 Key 加密在 U 盘内（Argon2id + AES-GCM），默认只监听 127.0.0.1`n`n用法：usb-bridge.exe 拷进 U 盘根目录；codex.exe 放 U 盘 bin\ 文件夹；launch-codex.bat 放 U 盘根目录（即「启动 Codex.bat」）→ 双击即用。"

    # 给 ASCII 名的附件打上中文 label（页面按 label 显示）
    try {
        $rel = gh api "repos/$Owner/$Repo/releases/tags/$Version" | ConvertFrom-Json
        foreach ($a in $rel.assets) {
            if ($a.name -eq "usage-guide.txt") {
                gh api "repos/$Owner/$Repo/releases/assets/$($a.id)" -X PATCH -f label="使用说明.txt" | Out-Null
            }
            if ($a.name -eq "launch-codex.bat") {
                gh api "repos/$Owner/$Repo/releases/assets/$($a.id)" -X PATCH -f label="启动 Codex.bat" | Out-Null
            }
        }
        Step "附件 label 已设置（使用说明.txt / 启动 Codex.bat）"
    } catch {
        Write-Host "[!] label 补丁失败（不影响下载，可在网页手动改）" -ForegroundColor Yellow
    }
}

# ---------------------------------------------------------------- 6) 收尾提示
Write-Host ""
Write-Host "[publish] 完成 ✅" -ForegroundColor Green
Write-Host "  仓库：      https://github.com/$Owner/$Repo"
Write-Host "  Releases：  https://github.com/$Owner/$Repo/releases"
Write-Host ""
Write-Host "还需在网页手动做一步（GitHub 不提供 API）：" -ForegroundColor Yellow
Write-Host "  Settings → Social preview → 上传 docs\social-preview.png"
