/** 管理后台通用工具函数 */

/** 统一错误提示文案：非 Error 一律归一为「请求失败」 */
export const errMsg = (e: unknown) => (e instanceof Error ? e.message : '请求失败')

/** IP 打码：IPv4 保留前两段，IPv6 保留前两组，其余打码 */
export function maskIp(ip: string) {
  if (!ip) return '—'
  if (ip.includes(':')) {
    // IPv6：保留前两段，其余打码
    const g = ip.split(':')
    return `${g[0]}:${g[1] || ''}:····`
  }
  const parts = ip.split('.')
  if (parts.length !== 4) return '…'
  return `${parts[0]}.${parts[1]}.*.*`
}

/**
 * 三端通用同一个 endpoint + token，只是安装命令按系统区分。
 *
 * allowExec 决定命令里带不带远程执行开关。默认不带——装了 agent 不等于同意被远程操作，
 * 而这个开关只能在装机时由执行命令的人决定，面板无法远程打开它。
 */
export function installCmds(token: string, allowExec = false) {
  const origin = window.location.origin
  return {
    sh:
      `curl -fsSL ${origin}/install.sh | bash -s -- --endpoint ${origin} --token ${token}` +
      (allowExec ? ' --allow-exec' : ''),
    ps:
      `powershell -ExecutionPolicy Bypass -Command "& ([scriptblock]::Create((iwr -useb ${origin}/install.ps1))) -Endpoint '${origin}' -Token '${token}'` +
      (allowExec ? ' -AllowExec' : '') +
      '"',
  }
}
