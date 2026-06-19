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

# 注册为开机自启计划任务（SYSTEM 账户运行，ICMP 探测需要该权限）
$taskName = "MossAgent"
schtasks /End /TN $taskName 2>$null | Out-Null
schtasks /Delete /TN $taskName /F 2>$null | Out-Null
schtasks /Create /TN $taskName /SC ONSTART /RU SYSTEM /RL HIGHEST /F `
  /TR "`"$bin`" --endpoint `"$Endpoint`" --token `"$Token`"" | Out-Null
schtasks /Run /TN $taskName | Out-Null

Write-Host "✅ 已安装并启动 Moss Agent（计划任务，开机自启）"
