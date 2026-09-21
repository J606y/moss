/**
 * 界面多语言：站点级选档，访客级解析。
 *
 * 档位（LangMode）存在服务端 settings，由管理员在「站点设置」里定，随公开接口
 * /api/site 下发——首页不登录就能看，访客必须拿得到。取 auto 时每位访客各自
 * 按浏览器语言解析，中文用户和外国用户同时打开同一个面板，各自看到看得懂的那一版。
 *
 * 状态用 useSyncExternalStore + 模块级订阅表，与 api/store.ts 同构，不引 i18n 框架：
 * 这套面板要的只是「查表 + 插值」，react-i18next 的命名空间/懒加载/复数规则一样不用。
 *
 * 首屏取 localStorage 里缓存的上次档位，不等 /api/site 回来。否则管理员把站点设成
 * English、而访客浏览器是中文时，首屏会先渲染成中文再跳成英文，闪一下。
 */
import { useCallback, useSyncExternalStore } from 'react'
import { safeLocalGet, safeLocalSet } from '../utils/storage'
import { zh, type Dict, type TextKey } from './zh'
import { en } from './en'

/** 站点选定的语言档位。auto = 跟随每位访客的浏览器。 */
export type LangMode = 'auto' | 'zh' | 'en'

/** auto 解析之后实际渲染用的语言。 */
export type Lang = 'zh' | 'en'

export type { TextKey }

const CACHE_KEY = 'moss-lang'

const dicts: Record<Lang, Dict> = { zh, en }

/** HTML lang 属性用的 BCP 47 标签，供屏幕阅读器与浏览器断词/翻译提示使用。 */
const htmlTag: Record<Lang, string> = { zh: 'zh-CN', en: 'en' }

export function parseMode(v: string | null | undefined): LangMode {
  return v === 'zh' || v === 'en' ? v : 'auto'
}

/**
 * auto 档的解析：取访客的首选语言，中文系归 zh，其余一律 en。
 *
 * 只看首选那一门，不在整个 languages 列表里找中文——列表里排在后面的往往是
 * 系统装的输入法或备选项，拿它做判据会把「首选英文、备选中文」的人判成中文。
 */
function detectLang(): Lang {
  if (typeof navigator === 'undefined') return 'zh'
  const prefs = navigator.languages?.length ? navigator.languages : [navigator.language]
  const first = prefs.find(Boolean)
  return first?.toLowerCase().startsWith('zh') ? 'zh' : 'en'
}

let mode: LangMode = parseMode(safeLocalGet(CACHE_KEY))
let lang: Lang = mode === 'auto' ? detectLang() : mode

const listeners = new Set<() => void>()

/**
 * 把语言写回 document：
 *   - `lang` 属性供屏幕阅读器选对发音、浏览器判断要不要提示翻译；
 *   - 标签页标题也是用户看得见的文案，英文界面下不该还挂着中文。
 *     标题只跟语言走，不取站点名称——那是管理员自己填的字符串，不该被翻译。
 */
function syncDocument() {
  if (typeof document === 'undefined') return
  document.documentElement.lang = htmlTag[lang]
  document.title = `Moss · ${dicts[lang]['brand.tagline']}`
}
syncDocument()

/** 落档并广播。语言没真变时不惊动任何组件，只更新档位本身。 */
function applyMode(next: LangMode) {
  mode = next
  const resolved = next === 'auto' ? detectLang() : next
  if (resolved === lang) return
  lang = resolved
  syncDocument()
  listeners.forEach((f) => f())
}

/** 站点档位变更入口：/api/site 拉取到、或管理员刚保存完设置时调用。 */
export function setSiteLang(next: LangMode) {
  safeLocalSet(CACHE_KEY, next)
  applyMode(next)
}

let started = false
function ensureStarted() {
  if (started) return
  started = true
  fetch('/api/site', { cache: 'no-store' })
    .then((r) => (r.ok ? (r.json() as Promise<{ lang?: string }>) : null))
    .then((d) => {
      if (d) setSiteLang(parseMode(d.lang))
    })
    .catch(() => {
      // 拉不到就继续用缓存档位（或浏览器判断的结果）。语言是展示层偏好，
      // 不值得为它阻塞首屏，也不该在离线时弹错。
    })
}

if (typeof window !== 'undefined') {
  // 跨标签页同步：另一个标签保存了新档位，本标签立即跟上，与主题的处理一致。
  window.addEventListener('storage', (e) => {
    if (e.key !== CACHE_KEY || e.newValue == null) return
    applyMode(parseMode(e.newValue))
  })
}

const subscribe = (cb: () => void) => {
  ensureStarted()
  listeners.add(cb)
  return () => {
    listeners.delete(cb)
  }
}

const getLangSnapshot = () => lang

/** 当前生效语言。供渲染期调用的纯函数（如 utils/format.ts）取用。 */
export const getLang = (): Lang => lang

/** 当前站点档位，auto 未解析。 */
export const getLangMode = (): LangMode => mode

export type TransParams = Record<string, string | number>

/**
 * 查表并插值。缺参数时原样保留 {name} 占位——界面上看得见，
 * 比悄悄留一段空白更容易在自测时发现。
 */
export function translate(l: Lang, key: TextKey, params?: TransParams): string {
  const tpl = dicts[l][key]
  if (!params) return tpl
  return tpl.replace(/\{(\w+)\}/g, (raw, name: string) => (name in params ? String(params[name]) : raw))
}

/**
 * 后端错误码 → 文案 key，表里没有这条码就返回 null。
 *
 * 拿 zh 当码的登记册而不是另列一张表：它本来就是基准表，多一张就多一处
 * 会不同步的地方。调用方拿到 null 时退回显示后端给的中文原文——所以后端
 * 新增一条码却忘了加翻译，界面只是不翻译，不会变成空白或 undefined。
 * 这条缝由 server 侧的一致性测试兜住（见 docs/error-codes.md 的 E 段）。
 */
export function errKey(code: string | null | undefined): TextKey | null {
  if (!code) return null
  const key = `err.${code}`
  return key in zh ? (key as TextKey) : null
}

/** 组件内取译文。语言切换时订阅者自动重渲染。 */
export function useT() {
  const current = useSyncExternalStore(subscribe, getLangSnapshot, getLangSnapshot)
  const t = useCallback(
    (key: TextKey, params?: TransParams) => translate(current, key, params),
    [current],
  )
  return { t, lang: current }
}
