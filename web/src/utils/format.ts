export function fmtBytes(n: number, digits = 1): string {
  if (!isFinite(n) || n <= 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB', 'PB']
  const i = Math.min(Math.floor(Math.log(n) / Math.log(1024)), units.length - 1)
  return `${(n / 1024 ** i).toFixed(i === 0 ? 0 : digits)} ${units[i]}`
}

export const fmtSpeed = (n: number) => `${fmtBytes(n)}/s`

export function fmtUptime(sec: number): string {
  if (sec <= 0) return '—'
  const d = Math.floor(sec / 86400)
  const h = Math.floor((sec % 86400) / 3600)
  const m = Math.floor((sec % 3600) / 60)
  if (d > 0) return `${d} 天 ${h} 时`
  if (h > 0) return `${h} 时 ${m} 分`
  return `${m} 分钟`
}

export const fmtPercent = (n: number) => `${(Number.isFinite(n) ? n : 0).toFixed(1)}%`

// Windows 的系统名取自注册表 ProductName，比 Linux 长一大截
// （"Microsoft Windows Server 2022 Datacenter 21H2" vs "Debian 13"），
// 列表里既撑宽表格又难读。此处剥掉三段无判读价值的填充：
//   1. "Microsoft " 前缀 —— 后面跟着 Windows，前缀不提供任何信息
//   2. 尾部 build 号 —— "10.0.20348 Build 20348"
//   3. Server 版本 SKU 及其后的 release ID —— "Datacenter 21H2"、"Standard 1809"
// 非 Windows 系统一律原样返回：Linux 发行版的版本号本身就是关键信息，
// 误剥会把 "Ubuntu 24.04" 砍成 "Ubuntu"。
// release ID 只在紧跟 SKU 时才剥：老版本用四位数形式（1809），与 Server 的
// 年份（2022）无法从字面区分，脱离 SKU 语境宁可保留也不误砍成 "Windows Server"。
export function shortOS(os: string): string {
  const s = os.replace(/^Microsoft\s+/i, '').trim()
  if (!/^Windows\b/i.test(s)) return s || os
  const trimmed = s
    .replace(/\s+\d+(\.\d+)+(\s+Build\s+\d+)?$/i, '')
    .replace(/\s+(Datacenter|Standard|Enterprise|Essentials)(\s+(?:\d{2}H\d|\d{4}))?$/i, '')
    .replace(/\s+\d{2}H\d$/i, '')
    .trim()
  return trimmed || s
}

export function fmtTime(ts: number): string {
  return new Date(ts).toLocaleTimeString('zh-CN', { hour12: false })
}

export function fmtDateTime(ts: number): string {
  return new Date(ts).toLocaleString('zh-CN', { hour12: false })
}

export function fmtAxisTime(ts: number, hours: number): string {
  const d = new Date(ts)
  if (hours > 24) return `${d.getMonth() + 1}/${d.getDate()} ${String(d.getHours()).padStart(2, '0')}时`
  return `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`
}
