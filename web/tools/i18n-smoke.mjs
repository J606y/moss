/**
 * 界面语言冒烟测试，跑在真实浏览器里。两件事：
 *
 *   1. 档位 × 浏览器语言的真值表 —— auto 跟随访客、zh/en 压过访客，是否真的成立。
 *   2. 英文界面不得残留汉字 —— 走遍首页、详情页与后台 8 个页签逐一扫描。
 *      第 2 条才是漏翻的真正闸门：`Dict` 类型只保证两张表 key 对齐，
 *      保证不了某条英文值里还留着中文。
 *   3. 后端错误码要真的翻得动 —— 认得的码出译文、认不出的码退回中文原文。
 *      单靠第 2 条盖不住：空串同样不含汉字，所以这里都是正向断言。
 *
 * 用法（先 npm run build）：
 *   node tools/i18n-smoke.mjs
 *
 * 接口一律用 route 拦截，不依赖后端真实状态——后端那一侧由
 * server/settings_test.go 覆盖。需要系统装了 Chrome 或 Edge。
 */
import { chromium } from 'playwright-core'
import { spawn } from 'node:child_process'
import { setTimeout as sleep } from 'node:timers/promises'
import { ADMIN_ROUTES, ADMIN_SERVER } from './fixtures.mjs'

const PORT = 4173
const BASE = `http://127.0.0.1:${PORT}`

/** 每一行都是一个「访客拿到什么语言」的断言。 */
const CASES = [
  { site: 'auto', locale: 'zh-CN', want: 'zh', why: 'auto + 中文浏览器 → 中文' },
  { site: 'auto', locale: 'en-US', want: 'en', why: 'auto + 英文浏览器 → 英文' },
  { site: 'auto', locale: 'de-DE', want: 'en', why: 'auto + 非中文浏览器 → 英文' },
  { site: 'zh', locale: 'en-US', want: 'zh', why: '站点强制中文，压过英文浏览器' },
  { site: 'en', locale: 'zh-CN', want: 'en', why: '站点强制英文，压过中文浏览器' },
  { site: 'wat', locale: 'en-US', want: 'en', why: '脏档位回落 auto，再按浏览器解析' },
]

/** 两门语言各自的判据：footer 的品牌副标题 + html lang 属性。 */
const EXPECT = {
  zh: { tagline: '智控中心', htmlLang: 'zh-CN' },
  en: { tagline: 'Control Center', htmlLang: 'en' },
}

/** 后台页签，按英文标签点击。顺序与 Admin.tsx 的 tabs 一致。 */
const ADMIN_TABS = [
  'Servers',
  'Probes',
  'Alerts',
  'GCP guard',
  'AI access',
  'Exec audit',
  'Panel updates',
  'Settings',
]

const FAKE_SERVER = {
  ...ADMIN_SERVER,
  os: 'Debian 13',
  arch: 'x86_64',
  virtualization: 'kvm',
  cpuModel: 'AMD EPYC 7B13',
  cpuCores: 2,
  memTotal: 4 * 1024 ** 3,
  swapTotal: 1024 ** 3,
  diskTotal: 40 * 1024 ** 3,
  intervalSec: 2,
  uptimeSec: 3 * 86400 + 5 * 3600,
  stats: {
    cpu: 12.5,
    memUsed: 2 * 1024 ** 3,
    swapUsed: 0,
    diskUsed: 12 * 1024 ** 3,
    netUp: 1024 * 512,
    netDown: 1024 * 900,
    totalUp: 12 * 1024 ** 3,
    totalDown: 40 * 1024 ** 3,
    tcp: 42,
    processes: 96,
    load1: 0.31,
    load5: 0.22,
    load15: 0.18,
  },
}

/** 扫一页的文本节点与常见属性，返回所有含汉字的串。 */
async function scanStray(page) {
  return page.evaluate(() => {
    const out = []
    const han = /\p{Script=Han}/u
    const walk = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT)
    for (let n = walk.nextNode(); n; n = walk.nextNode()) {
      const s = n.textContent?.trim()
      if (s && han.test(s)) out.push(s)
    }
    for (const el of document.querySelectorAll('[title],[placeholder],[aria-label]')) {
      for (const attr of ['title', 'placeholder', 'aria-label']) {
        const v = el.getAttribute(attr)
        if (v && han.test(v)) out.push(`@${attr}=${v}`)
      }
    }
    return [...new Set(out)]
  })
}

/** 建一个上下文并把所有接口挡掉。site 决定站点语言档位。 */
async function newContext(browser, { locale, site }) {
  const ctx = await browser.newContext({ locale, viewport: { width: 1440, height: 900 } })
  await ctx.route('**/api/site', (r) =>
    r.fulfill({ json: { name: 'Moss', desc: 'Control Center', lang: site } }),
  )
  await ctx.route('**/api/servers', (r) => r.fulfill({ json: [FAKE_SERVER] }))
  await ctx.route('**/api/servers/*/recent', (r) => r.fulfill({ json: [] }))
  await ctx.route('**/api/servers/*/history**', (r) => r.fulfill({ json: [] }))
  await ctx.route('**/api/servers/*/ping**', (r) => r.fulfill({ json: { tasks: [], series: {} } }))
  for (const [path, body] of ADMIN_ROUTES) {
    await ctx.route(`**${path}**`, (r) => r.fulfill({ json: body }))
  }
  // WebSocket 连不上不影响渲染，store 会自己退避重连
  return ctx
}

const results = []
const record = (ok, why, detail) => results.push({ ok, why, detail })

const preview = spawn('npx', ['vite', 'preview', '--port', String(PORT), '--strictPort'], {
  stdio: 'ignore',
  shell: true,
})

async function waitForServer(url, timeoutMs = 20000) {
  const deadline = Date.now() + timeoutMs
  while (Date.now() < deadline) {
    try {
      const r = await fetch(url)
      if (r.ok) return
    } catch {
      // 还没起来，继续等
    }
    await sleep(200)
  }
  throw new Error(`预览服务 ${url} 在 ${timeoutMs}ms 内没起来`)
}

async function launchBrowser() {
  for (const channel of ['chrome', 'msedge']) {
    try {
      return await chromium.launch({ channel, headless: true })
    } catch {
      // 换下一个；两个都没有才算真的跑不起来
    }
  }
  throw new Error('未找到系统 Chrome 或 Edge，无法实测')
}

let browser
try {
  await waitForServer(BASE)
  browser = await launchBrowser()

  /* ---------- 1. 档位真值表 ---------- */
  for (const c of CASES) {
    const ctx = await newContext(browser, c)
    const page = await ctx.newPage()
    await page.goto(BASE, { waitUntil: 'networkidle' })

    const want = EXPECT[c.want]
    const other = EXPECT[c.want === 'zh' ? 'en' : 'zh']
    const footer = (await page.locator('footer').innerText()).trim()
    const htmlLang = await page.getAttribute('html', 'lang')
    const title = await page.title()

    const problems = []
    if (!footer.includes(want.tagline)) problems.push(`footer 应含「${want.tagline}」，实际 "${footer}"`)
    if (footer.includes(other.tagline)) problems.push(`footer 混进了另一门语言："${footer}"`)
    if (htmlLang !== want.htmlLang) problems.push(`html lang 应为 ${want.htmlLang}，实际 ${htmlLang}`)
    if (!title.includes(want.tagline)) problems.push(`标签页标题应含「${want.tagline}」，实际 "${title}"`)

    record(problems.length === 0, c.why, problems)
    await ctx.close()
  }

  /* ---------- 2. 跨标签同步 ---------- */
  {
    const ctx = await newContext(browser, { locale: 'zh-CN', site: 'auto' })
    const page = await ctx.newPage()
    await page.goto(BASE, { waitUntil: 'networkidle' })
    await page.evaluate(() => {
      localStorage.setItem('moss-lang', 'en')
      window.dispatchEvent(new StorageEvent('storage', { key: 'moss-lang', newValue: 'en' }))
    })
    await page
      .waitForFunction(() => document.documentElement.lang === 'en', null, { timeout: 3000 })
      .catch(() => {})
    const footer = (await page.locator('footer').innerText()).trim()
    record(footer.includes('Control Center'), 'storage 事件到达时当场换语言，不必刷新', [
      `footer 仍是 "${footer}"`,
    ])
    await ctx.close()
  }

  /* ---------- 3. 英文界面全站扫汉字 ---------- */
  {
    // 站点强制英文，浏览器仍是中文：任何一处漏翻都躲不过去
    const ctx = await newContext(browser, { locale: 'zh-CN', site: 'en' })
    const page = await ctx.newPage()

    const scan = async (where) => {
      const stray = await scanStray(page)
      record(stray.length === 0, `英文界面无汉字 · ${where}`, [stray.slice(0, 6).join(' / ')])
    }

    await page.goto(BASE, { waitUntil: 'networkidle' })
    await scan('首页 卡片视图')

    await page.locator('header ~ main button[title="List view"], button[title="List view"]').first().click()
    await page.waitForTimeout(150)
    await scan('首页 列表视图')

    await page.goto(`${BASE}/server/demo-1`, { waitUntil: 'networkidle' })
    await scan('详情页 负载·实时')

    await page.getByRole('button', { name: '1 hour', exact: true }).click()
    await page.waitForTimeout(250)
    await scan('详情页 负载·历史')

    await page.getByRole('button', { name: 'Latency', exact: true }).click()
    await page.waitForTimeout(250)
    await scan('详情页 延迟')

    await page.goto(`${BASE}/admin`, { waitUntil: 'networkidle' })
    for (const tab of ADMIN_TABS) {
      await page.locator('aside').getByRole('button', { name: tab, exact: true }).click()
      await page.waitForTimeout(250)
      await scan(`后台 ${tab}`)

      // 「没有汉字」拦不住「什么都没有」——空串同样不含汉字。那两处由错误码
      // 渲染的提示因此还要一条正向断言：译文出来了，且不翻译的 detail 没被吞掉。
      if (tab === 'Servers') {
        const titles = await page
          .locator('[title]')
          .evaluateAll((els) => els.map((e) => e.getAttribute('title') ?? ''))
        const hit = titles.find((s) => s.includes('v1.4.0')) ?? ''
        record(hit.includes('too old'), '升级提示按错误码译成英文，版本号原样保留', [`title="${hit}"`])
      }
      if (tab === 'Panel updates') {
        const body = await page.locator('main').innerText()
        record(
          body.includes('offline') && body.includes('old-node'),
          '面板主机提示按错误码译成英文，机器名原样保留',
          [body.replace(/\s+/g, ' ').slice(0, 200)],
        )
      }
    }
    await ctx.close()
  }

  /* ---------- 4. 错误响应 → 界面文案 ---------- */
  {
    // 登录页是唯一不需要先登录就能触发错误的地方，拿它验三条路径。
    const ctx = await newContext(browser, { locale: 'zh-CN', site: 'en' })
    const page = await ctx.newPage()

    const loginErr = async (body) => {
      await ctx.unroute('**/api/login')
      await ctx.route('**/api/login', (r) => r.fulfill({ status: 401, json: body }))
      await page.goto(`${BASE}/login`, { waitUntil: 'networkidle' })
      await page.getByPlaceholder('Enter password', { exact: true }).fill('nope')
      await page.getByRole('button', { name: 'Sign in', exact: true }).click()
      const p = page.locator('form p.text-rose-500')
      await p.waitFor({ state: 'visible', timeout: 3000 }).catch(() => {})
      return (await p.innerText().catch(() => '')).trim()
    }

    const known = await loginErr({ error: '用户名或密码错误', code: 'auth.bad_credentials' })
    record(known === 'Incorrect username or password', '认得的错误码译成英文', [`实际 "${known}"`])

    // 后端新增了码却忘了补文案时的兜底：显示后端的中文原文。
    // 难看，但看得懂；退化成空白或 undefined 才是真的坏掉。
    const unknown = await loginErr({ error: '服务端新加的某种错误', code: 'brand.new_code' })
    record(unknown === '服务端新加的某种错误', '认不出的码退回后端原文，不留空白', [`实际 "${unknown}"`])

    // 老接口没有 code 字段，行为必须和改造前一模一样。
    const bare = await loginErr({ error: '没有码的老响应' })
    record(bare === '没有码的老响应', '无 code 的响应照旧显示 error 原文', [`实际 "${bare}"`])

    await ctx.close()
  }
} finally {
  await browser?.close()
  preview.kill()
}

let failed = 0
for (const r of results) {
  if (r.ok) {
    console.log(`ok    ${r.why}`)
  } else {
    failed++
    console.log(`FAIL  ${r.why}`)
    for (const d of r.detail.filter(Boolean)) console.log(`      ${d}`)
  }
}
console.log(failed ? `\n${failed} 项失败` : `\n全部通过（${results.length} 项）`)
process.exit(failed ? 1 : 0)
