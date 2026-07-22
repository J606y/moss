# Moss Agent 安装脚本（Windows）
# 用法: powershell -ExecutionPolicy Bypass -Command "& ([scriptblock]::Create((iwr -useb https://your-moss/install.ps1))) -Endpoint 'https://your-moss' -Token 'mk_xxx'"
param(
  [Parameter(Mandatory = $true)][string]$Endpoint,
  [Parameter(Mandatory = $true)][string]$Token,
  [string]$Repo = "j606y/moss",
  [string]$Version = "latest"
)
$ErrorActionPreference = "Stop"

$arch = if ([Environment]::Is64BitOperatingSystem) {
  if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") { "arm64" } else { "amd64" }
} else { throw "不支持 32 位系统" }

$dir = "$env:ProgramFiles\moss-agent"
$bin = "$dir\moss-agent.exe"
New-Item -ItemType Directory -Force $dir | Out-Null

if ($Version -eq "latest") {
  $url = "https://github.com/$Repo/releases/latest/download/moss-agent-windows-$arch.exe"
} else {
  $url = "https://github.com/$Repo/releases/download/$Version/moss-agent-windows-$arch.exe"
}

Write-Host "下载 $url ..."
Invoke-WebRequest -UseBasicParsing -Uri $url -OutFile $bin

# 完整性校验：release 附带 SHA256SUMS。缺失则告警但继续，不匹配则终止。
$sumsUrl = ($url -replace '[^/]+$', 'SHA256SUMS')
$fileName = ($url -split '/')[-1]
$expect = $null
try {
  $sums = (Invoke-WebRequest -UseBasicParsing -Uri $sumsUrl).Content
  $expect = ($sums -split "`n" | Where-Object { $_ -match ([regex]::Escape($fileName) + '$') } | ForEach-Object { ($_ -split '\s+')[0] } | Select-Object -First 1)
} catch {
  Write-Host "⚠️  校验和获取失败，跳过完整性校验"
}
if ($expect) {
  $actual = (Get-FileHash -Algorithm SHA256 -Path $bin).Hash.ToLower()
  if ($actual -ne $expect.ToLower()) {
    Remove-Item $bin -Force
    throw "❌ 校验和不匹配，终止安装（期望 $expect，实际 $actual）"
  }
  Write-Host "✅ 校验和匹配"
} else {
  Write-Host "⚠️  未获取到 SHA256SUMS，跳过完整性校验"
}

# token 写入受限文件（仅 SYSTEM 与 Administrators 可读），避免出现在计划任务命令行 / schtasks /Query /V
$tokenPath = "$dir\token"
# 上次安装若已把 token 收紧成只读（仅 R），覆盖写会被拒。先给管理员组恢复完全控制，
# 保证重装可重复执行；写完后第 52 行会再统一收紧权限。
if (Test-Path $tokenPath) {
  $ErrorActionPreference = "Continue"
  icacls $tokenPath /grant "Administrators:(F)" 2>$null | Out-Null
  $ErrorActionPreference = "Stop"
}
Set-Content -Path $tokenPath -Value $Token -NoNewline -Encoding ascii
icacls $tokenPath /inheritance:r /grant "SYSTEM:R" "Administrators:R" | Out-Null

# 注册为开机自启计划任务（SYSTEM 账户运行，ICMP 探测需要该权限）
$taskName = "MossAgent"
# 首次安装时任务不存在，schtasks 会往 stderr 写「找不到文件」；在 Stop 模式下这会被
# 当成终止性 NativeCommandError（2>$null 也拦不住），导致任务未创建就退出。故临时降级。
$ErrorActionPreference = "Continue"
schtasks /End /TN $taskName 2>$null | Out-Null
schtasks /Delete /TN $taskName /F 2>$null | Out-Null
$ErrorActionPreference = "Stop"
schtasks /Create /TN $taskName /SC ONSTART /RU SYSTEM /RL HIGHEST /F `
  /TR "`"$bin`" --endpoint `"$Endpoint`" --token-file `"$tokenPath`"" | Out-Null
schtasks /Run /TN $taskName | Out-Null

Write-Host "✅ 已安装并启动 Moss Agent（计划任务，开机自启）"
