# API 错误码

> **接手须知**：A–E 五段全部完成并验证（Go 四项全绿 + 浏览器冒烟 25 项全绿）。
> 本次改造收尾，剩余的已知缺口见 [i18n.md](./i18n.md) 的「已知缺口」。详见文末「当前状态」。

## 为什么做

界面已经支持中英文（见 [i18n.md](./i18n.md)），但后端错误信息仍是中文，直接经 `error` 字段流到界面 —— 英文界面下一报错就冒中文。

真正疼的不是难看，是那几条**把处置办法写进错误信息**的。最典型的一条：

```
该凭证无法解密，主密钥可能已变更；请恢复原 MOSS_SECRET_KEY 或 secret.key，或删除后重新添加
```

这句里唯一的自救路径读不懂，用户看见的只是一串方块字和一个 Delete 按钮 —— 然后就点了，而删掉即永久失去那份私钥。

## 为什么是错误码，不是前端文本映射

前端也能拿中文串查表换成英文，不用动后端。但那样**中文串就是 key** —— 它是自然语言，会变。后端改一个字，映射静默失效，退回中文且无人发现。加第三种语言时，每种语言都要再绑一次这批中文原文，脆弱性随语言数量放大。

错误码是语言中立的，`auth.bad_credentials` 不会因为谁改了措辞而失效。加日语＝在 `ja.ts` 多翻一份，后端一行不用动。

另有一项白赚的收益：`/mcp` 那头是 AI 在消费这些错误，稳定的码比中文字面更好判断分支。

## 契约

### 响应体

```json
{ "error": "凭证无效：invalid PEM", "code": "gcp.cred_invalid", "detail": "invalid PEM" }
```

- `error` —— 中文兜底，**始终存在**。前端认不出 `code` 就显示它。所以漏一条码只是「不翻译」，不会变成空白或 `undefined`。
- `code` —— 语言中立标识，空则不下发该字段。
- `detail` —— 不翻译的那截（机器名、Service Account 邮箱、Go error 原文）。

### 两个类型（`server/apierr.go`）

| | 用途 |
|---|---|
| `apiErr{Status, Code, Msg, Detail}` | HTTP 出口。`writeErr(w, errXxx)` |
| `codedError{Code, Msg, Detail}` | 底层 error。`Error()` 仍返回中文，**日志 / MCP 返回 / 审计记录全不受影响** |

底层的码经 `fmt.Errorf("%w")` 包装后仍能被 `errors.As` 挖出来，出口用 `writeErrFrom(w, status, err, fallback)` 自动提取。

### 硬约定

1. **细节一律放末尾走 `Detail`，`Msg` 以冒号收尾。** 不要在 `Msg` 里留 `%s` 占位 —— 漏调一次 `.with()` 就会把 `%!s(MISSING)` 发到界面上。原文里细节在句中的（「agent v1.0 过旧」），措辞改成末尾（「agent 过旧……，当前 v1.0」）。
2. **新增错误必须先在 `apierr.go` 的错误表登记**，不要随手 `writeErr` 一句中文 —— 那会把界面又拉回不可翻译的状态。
3. **码的分段前缀**按归属：`auth.` / `gcp.` / `upgrade.` / `panel.` / `webhook.` / `notify.` / `key.` / `server.` / `audit.`，无归属的用裸名（`internal`、`bad_json`）。

## 进度

- [x] **A 机制** — `server/apierr.go`：两个类型 + `writeErr` / `writeErrFrom` + 约 45 条错误表
- [x] **B 后端迁移** — 119 处 `writeErr` 全部迁完，静态扫描确认无旧签名残留。三个深层来源一并改造：
  - `normalizeGCP` 返回 `*apiErr`（4 条）
  - `panelHostReady` 返回 `*apiErr`（4 条）
  - `upgradeAvailability` 返回 `*codedError`（5 条）；它的提示还挂在 `AdminServer.upgradeHint` 上给前端显示按钮 title，所以响应加了 `upgradeHintCode` / `upgradeHintDetail`
  - 同理 `panelUpdateView` 加了 `hostHintCode` / `hostHintDetail`（`hostHint` 也是既进界面又进错误响应）
  - 顺手：`auth.go` 原本把 SQL 错误原文回给客户端（信息泄露 + 没人看得懂），改成记日志 + `errInternal`
  - 测试跟随改造：`TestNormalizeGCPCredential` 与 `upgrade_test.go` 的可用性断言从「中文子串匹配」改成比对 `Code`。措辞会改、码不会，这是 E 段那条一致性测试的同一个理由
- [x] **C 前端消费** — 见下，55 条码全线打通，冒烟 25 项全绿
- [x] **D 通知码化** — 见下，指标常量化 + 告警文案双语，新增 6 项测试
- [x] **E 一致性测试** — 见下，新增 2 项测试；码与文案表双向对齐，`{detail}` 占位符对齐

### C 前端消费（已完成）

1. `web/src/api/client.ts` —— `code` / `detail` 从响应体带到 `ApiError` 上。
2. `web/src/i18n/index.ts` 的 `errKey(code)` —— 码 → 文案 key，表里没有就返回 `null`。拿 `zh` 当码的登记册，不另列一张会不同步的表。
3. `web/src/utils/admin.ts` —— 内部的 `coded()` 收口翻译逻辑，对外两个出口：
   - `errMsg(e)` 给所有 toast 与 `catch`；
   - `hintText(hint, code, detail)` 给 `upgradeHint` / `hostHint` 这类「既进界面也进错误响应」的字段。
     两者共用 `coded()`，免得日后只改一处。
4. `web/src/i18n/zh.ts` / `en.ts` —— **55 条** `err.*`，与后端错误表逐条对应。中文照抄 `Msg`。
5. `types.ts` / `ServersTab.tsx` / `PanelUpdateTab.tsx` / `Login.tsx` —— 消费上述字段。

**顺带收掉的尾巴：三条 2xx 成功消息**（`gcp-start`、`upgrade`、`panel-update/start`）。它们不走错误码契约，但同样是后端返的中文，英文界面照样冒中文。改为前端按结构化字段自己渲染（`gcp-start` 用它已有的 `status` / `started` 判三种结局），**后端一行没动** —— `message` 字段保留给 API / MCP 消费者。

**未做，理由明确的两处：**

- `PanelUpdateTab` 的 `checkError` / `avail.reason` / `stageErr`：GitHub 查询与部署脚本的动态文本，没有有限的码集合。
- `AuditTab.tsx:255` 的 `row.error.includes('拦截')`：**已查证 `exec_audit` 表没有 `code` 列**，后端自己的「仅看拦截」筛选也是 `error LIKE '命令被拦截：%'`（`execBlockedPrefix`）。这里存的是当初落库的历史文本，不是错误响应，所以**不该**跟着 HTTP 错误码改。要改得先加列并回填，归到 D 段。代码里的注释已按此更正。

### D 通知码化（已完成）

根源问题：`notify.go` 里 `metric` 的取值是 `"CPU"` / `"内存"` / `"硬盘"` / `"net"` —— 中英混杂，且**一个字符串身兼三职**：内部状态机的 key、Webhook payload 的契约字段、拼进 Telegram 中文消息。改一个字状态机就对不上，而编译器完全不管。

做法与落点：

1. **指标常量化** —— `metricCPU` / `metricMem` / `metricDisk` / `metricNet` 定在 `server/webhook.go`，与事件类型并列（它本就是对外契约字段）。取值与 MCP `get_history` 的 `historyMetricList` 一致，对端只需认一套词。
2. **文案外提** —— 新增 `server/alert_text.go`：13 条文案 × 2 语言，比照 `apierr.go` 集中登记。告警产生处只填 `params` 与 `textKey`，一句人话也不拼。
3. **出口渲染** —— `Notifier.fire()` 调 `renderAlert(ev, n.alertLang())`，Telegram 与 webhook 拿同一句话。`alertEvent` 新增两个**不导出**字段 `textKey` / `params`，因此不进 JSON：模板参数是渲染的内部细节，暴露出去只会变成新的隐式契约。
4. **语言现读不缓存** —— `alertLang()` 每次查 settings。因为保存站点设置**不会**调 `notifier.Reload()`，缓存会一直陈旧到进程重启；而告警是低频事件，同一条路径上的 `serverName` 本来就每次查一次库。

两个设计细节：

- **`textKey` 解决「同一 Type、不同措辞」**：`server.expiring` 的 0 天要说「今天到期」而不是「剩余 0 天」；`gcp.autostart_failed` 的「查询状态失败」与「start 失败」也不是同一句。Type 是对外契约不能为措辞分裂，所以另起一个只在内部用的 key。
- **`{metric}` 由 `Metric` 码现查**，不让产生处把人话塞进 `params` —— 那等于又把语言相关的东西带回了告警产生处。

事件类型 `alertEvent.Type` 本就语言中立（`server.load_alert`），**没动** —— 它是这个项目里错误码的先例。

**不翻译的两处**：`exec.blocked` 的 `reason`（拦截规则给出的原因）与 `cmd`（用户/AI 敲的原文）。与错误码里 `Detail` 同理——那是证据，翻译反而会改变它。

> **破坏性变更**：`metric` 字段值从「内存」变成 `mem`；`text` 字段随站点语言变化。已在 CHANGELOG 的「未发布」段注明，`docs/ai-ops.md` 的 webhook 契约同步更新。

### E 一致性测试（已完成）

1. **[x] 码与翻译的一致性测试** —— `server/apierr_test.go`，两项：

   | 测试 | 抓的是 |
   |---|---|
   | `TestErrorCodesHaveTranslations` | 双向比对后端码与 `zh.ts` / `en.ts` 的 `err.*`。后端多一条 → 英文界面冒中文；文案表多一条 → 码拼错或已删除 |
   | `TestErrorDetailPlaceholdersAligned` | 后端 `Msg` 以冒号（或实词+空格）收尾即带 Detail，文案必须有 `{detail}`。少一个占位符，机器名 / 版本号被**静默吞掉** |

   码清单**从源码里扫**（`*.go` 去掉 `_test.go`），不在测试里手抄——手抄那份不会因为别人新增一条 `apiErr{Code:...}` 而失败。`Msg` 写成常量的条目（目前只有 `upgradeOSUnsupportedHint`）由同一轮扫出的常量表还原，不特判。

   扫到的码少于 40 条即 `t.Fatal`：正则与源码写法一旦脱节，「全部通过」就是假的。

   第二项抓的那类错，`Dict` 类型抓不到，冒烟的「英文界面无汉字」也抓不到（少一个占位符照样没有汉字），实测确认过。

2. **[x] 冒烟测试扩展** —— 已随 C 做完，`web/tools/i18n-smoke.mjs` 从 20 项增至 25 项。
3. **[x] 全量跑** —— 见「当前状态」。

## 当前状态

**A–E 五段全部验证通过**（2026-09-21）。本次改造收尾。

```bash
cd "E:/桌面/moss" && gofmt -l . ; go build ./... && go vet ./... && go test ./...
cd web && npm run verify        # tsc → vite build → 浏览器冒烟 25 项
```

两个必须知道的坑：

- **`go vet` 不能省** —— `go build` 不编译 `_test.go`，两处按旧签名写的测试只有 vet / test 才抓得到。
- **别单跑 `npm run test:i18n`** —— 它不重新构建，会拿旧的 `dist/` 跑，改了源码照样全绿。一律走 `npm run verify`。（对照组期间踩过。）

### 冒烟测试新增的 5 项（C 段）

除「英文界面无汉字」外，另加了**正向**断言：无汉字拦不住「什么都没有」，空串同样不含汉字。

| 断言 | 抓的是 |
|---|---|
| 升级提示按错误码译成英文，版本号原样保留 | `hintText` + `{detail}` 插值 |
| 面板主机提示按错误码译成英文，机器名原样保留 | 同上，另一条路径 |
| 认得的错误码译成英文 | `errMsg` 主路径 |
| 认不出的码退回后端原文，不留空白 | 漏登记时的兜底 |
| 无 `code` 的响应照旧显示 `error` 原文 | 与改造前行为一致 |

假数据里 `upgradeHint` / `hostHint` **故意写成中文**（`web/tools/fixtures.mjs`）：那是后端兜底串，翻译正常时永远不该出现在英文界面上；一旦扫描扫出它，就是翻译那条路断了。这与该文件「假数据一律用英文」的规则冲突，属唯一的有意例外，文件头已注明。

两轮对照组实测（改完必跑，确认测试真抓得到 bug）：

- 让 `errKey` 恒返回 `null` → 精确失败 5 项，两条兜底断言仍绿（它们本就走不翻译的路径）。
- 抽掉 `en.ts` 里一个 `{detail}` → 只有那条正向断言失败，而「后台 Servers 无汉字」**仍是绿的** —— 正是这条对照组说明了正向断言不可省。

### D 段新增的 6 项 Go 测试（`server/alert_text_test.go`）

| 测试 | 抓的是 |
|---|---|
| `TestAlertTextsCoverAllEventTypes` | 漏登记一条文案 → 推送**一条空消息**（产生处已不再拼文案） |
| `TestAlertTextPlaceholdersMatchAcrossLangs` | 英文少一个占位符 → 那个参数被静默吞掉 |
| `TestAlertTextsNonEmpty` | 某种语言留空 |
| `TestAlertTextEnglishHasNoHan` | 照抄中文忘了译 |
| `TestRenderAlert` | 渲染本身：双语、`textKey` 变体、缺参数保留占位符、未登记类型的兜底 |
| `TestFireRendersBySiteLang` | 端到端：`fire` 真的去读了站点语言（zh / en / auto 三档） |

第一条的类型清单**从 `webhook.go` 源码里扫**，不在测试里手抄——手抄那份不会因为别人新增 `evtXxx` 而失败，等于没有这道闸。这也正是 E1 要用的做法。

对照组实测：一次植入三个 bug（删一条文案 / 抽掉一个英文占位符 / 在英文里塞一个汉字），三条对应断言分别精确报错，另有两项连带失败。

### B 段首次编译暴露并修掉的三处

- `panel_update.go` 的 `writePanelUpdateView` 仍按旧的 `string` 接 `panelHostReady` 的返回（唯一一处真编译错误）。顺带给 `panelUpdateView` 加了 `hostHintCode` / `hostHintDetail`，否则码在出口被丢掉，C 段无从消费。
- `gcp_credentials_test.go` / `upgrade_test.go` 仍按旧签名断言中文子串 —— 改成比对 `Code`。
- `admin_api.go` / `panel_update.go` 各有一处改造时留下的重复注释块，已合并。

交接文档写的两条风险实跑后均未发生：`panel_update_test.go` 的「更高」/「离线」断言仍通过；`mcp_test.go` 不受影响。

## 已知取舍

- 两处 `errGCPCredDuplicate` 原本是两条不同措辞（一条带「如需更换密钥……」），已合并成同一个码，文案取更完整的那条。这是有意的。
- `panelUpdateView` 的 `checkError` / `avail.reason` / `stageErr` 仍是中文动态文本，本次不码化（理由见 C 段第 5 条）。
