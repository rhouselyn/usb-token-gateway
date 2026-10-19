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

# ---------------------------------------------------------------- 0) 检查 gh
if (-not (Get-Command gh -ErrorAction SilentlyContinue)) {
    Fail "未找到 gh。先执行：winget install GitHub.cli（装完重开终端）"
}
cmd /c "gh auth status >nul 2>nul"
if ($LASTEXITCODE -ne 0) {
    Fail "gh 未登录。先执行：gh auth login -h github.com -p https -w"
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
cmd /c "git rev-parse --verify HEAD >nul 2>nul"
if ($LASTEXITCODE -ne 0) {
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
cmd /c "gh repo view $Owner/$Repo >nul 2>nul"
if ($LASTEXITCODE -eq 0) {
    Step "仓库已存在，直接推送"
    cmd /c "git remote get-url origin >nul 2>nul"
    if ($LASTEXITCODE -ne 0) { git remote add origin "https://github.com/$Owner/$Repo.git" }
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
    $assets = @()
    foreach ($f in @("usb\usb-bridge.exe", "usb\使用说明.txt")) {
        $p = Join-Path $Root $f
        if (Test-Path $p) { $assets += $p } else { Write-Host "[!] 缺少 $f（跳过该附件）" -ForegroundColor Yellow }
    }
    if (-not $assets) { Fail "没有可作为附件的文件" }
    Step "发布 $Version（附件：$($assets.Count) 个）"
    gh release create $Version @assets --title "$Version — U盘即插即用的本地 LLM token 中转站" `
        --notes "双击即用：首次设一次主密码，之后静默启动、不弹浏览器。`n`n- OpenAI 兼容端点：``/v1/chat/completions``、``/v1/models``、``/v1/embeddings```n- Anthropic 原生端点：``/v1/messages``（含 SSE 流式）`n- 多渠道轮换 / 熔断 / 429 冷却`n- 上游 Key 加密在 U 盘内（Argon2id + AES-GCM），默认只监听 127.0.0.1`n`n用法：把 ``usb-bridge.exe`` 拷进 U 盘 → 插到任意 Windows 电脑 → 双击。"
}

# ---------------------------------------------------------------- 6) 收尾提示
Write-Host ""
Write-Host "[publish] 完成 ✅" -ForegroundColor Green
Write-Host "  仓库：      https://github.com/$Owner/$Repo"
Write-Host "  Releases：  https://github.com/$Owner/$Repo/releases"
Write-Host ""
Write-Host "还需在网页手动做一步（GitHub 不提供 API）：" -ForegroundColor Yellow
Write-Host "  Settings → Social preview → 上传 docs\social-preview.png"
