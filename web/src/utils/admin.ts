/** 管理后台通用工具函数 */
import type { ApiError } from '../api/client'
import { errKey, getLang, translate } from '../i18n'

/**
 * 错误码 → 当前语言的文案；认不出这条码就用 fallback（后端给的中文原文）。
 *
 * {detail} 接住机器名、版本号一类不翻译的尾巴，后端没给就替换成空串——
 * 与后端自己拼 Msg+Detail 的结果一致。
 */
const coded = (code: string | undefined, detail: string | undefined, fallback: string) => {
  const key = errKey(code)
  return key ? translate(getLang(), key, { detail: detail ?? '' }) : fallback
}

/**
 * 统一错误提示文案：非 Error 一律归一为「请求失败」。
 *
 * 认不出的码——后端新增了却忘了补文案，或压根不是结构化错误（网络失败、
 * 非 JSON 响应）——退回 e.message，也就是后端给的中文原文。漏一条只是
 * 不翻译，不会变成空白。契约见 docs/error-codes.md。
 */
export const errMsg = (e: unknown) => {
  if (!(e instanceof Error)) return translate(getLang(), 'common.requestFailed')
  const { code, detail } = e as ApiError
  return coded(code, detail, e.message)
}

/**
 * 后端那类「既进界面也进错误响应」的提示：AdminServer.upgradeHint、
 * PanelUpdate.hostHint。它们不经 throw，拿不到 ApiError，但字段同形，
 * 所以复用同一套翻译逻辑，免得两处各写一遍、日后只改一处。
 */
export const hintText = (hint = '', code?: string, detail?: string) => coded(code, detail, hint)

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
