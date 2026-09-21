// 告警文案：把「发生了什么」与「怎么说」分开。
//
// 告警产生处只填结构化字段和参数，一句人话也不拼；文案在出口按语言渲染。
//
// 直接原因是推送这条路没有浏览器参与——Telegram 那头的语言只能由后端决定，
// 前端那套 i18n 够不着。但更要紧的收益是顺手拆开了一个身兼三职的字符串：
// metric 此前既是状态机的 key（st.highSince[metric]）、又是 webhook 的契约字段、
// 还要被拼进中文消息。改一个字，状态机 key 就与 clearMetricState 对不上，
// 而编译器完全不管。契约见 docs/error-codes.md 的 D 段。
package main

import "strings"

// alertText 一条告警文案的两种语言。
//
// 结构与前端 i18n/zh.ts、en.ts 同构：占位符写 {name}，缺参数时原样保留——
// 推送里看得见，比悄悄留一段空白更容易在自测时发现。
type alertText struct{ zh, en string }

func (t alertText) pick(lang string) string {
	if lang == langEN {
		return t.en
	}
	return t.zh
}

// 同一事件类型下的文案变体。
//
// Type 是对外契约，不能为了措辞分裂；但措辞确实得分开：「今天到期」拼不出来
// （「剩余 0 天」很别扭），「查询实例状态失败」与「start 失败」也不是同一句话。
const (
	txtExpiringToday   = evtServerExpiring + ".today"
	txtGCPFailedStatus = evtGCPFailed + ".status"
)

// alertTexts 全部告警文案。key 取 alertEvent.textKey，为空时即事件类型。
//
// 英文一律不做单复数分支，用措辞规避（与前端同一条硬约定）：数字要么后缀不变
// （{sec}s、{min} min、attempt {tries}/{max}），要么从名词旁挪开
// （Days left: {days}）。写 day(s) 一律算没做完。
var alertTexts = map[string]alertText{
	evtServerOnline: {
		zh: "🟢 服务器恢复\n{name} 已重新上线（离线 {dur}）",
		en: "🟢 Server back online\n{name} reconnected after {dur} offline",
	},
	evtServerOffline: {
		zh: "🔴 服务器离线\n{name} 已离线超过 {sec} 秒",
		en: "🔴 Server offline\n{name} has been offline for over {sec}s",
	},

	evtLoadAlert: {
		zh: "⚠️ 负载告警\n{name} {metric} 使用率 {val}%，已持续 {min} 分钟（阈值 {th}%）",
		en: "⚠️ Load alert\n{name} {metric} usage {val}%, sustained for {min} min (threshold {th}%)",
	},
	evtLoadRecovered: {
		zh: "✅ 负载恢复\n{name} {metric} 已回落至 {val}% 并持续 {sec} 秒",
		en: "✅ Load recovered\n{name} {metric} fell back to {val}% and held for {sec}s",
	},
	evtNetAlert: {
		zh: "⚠️ 网速告警\n{name} 上行 {up} MB/s / 下行 {down} MB/s，已持续 {sec} 秒（阈值 {th} MB/s）",
		en: "⚠️ Network alert\n{name} up {up} MB/s / down {down} MB/s, sustained for {sec}s (threshold {th} MB/s)",
	},
	evtNetRecovered: {
		zh: "✅ 网速恢复\n{name} 网速已回落至 ↑ {up} / ↓ {down} MB/s 并持续 {sec} 秒",
		en: "✅ Network recovered\n{name} fell back to ↑ {up} / ↓ {down} MB/s and held for {sec}s",
	},

	evtServerExpiring: {
		zh: "📅 到期提醒\n{name} 将于 {date} 到期（剩余 {days} 天）",
		en: "📅 Expiry reminder\n{name} expires on {date}. Days left: {days}",
	},
	txtExpiringToday: {
		zh: "📅 到期提醒\n{name} 今天到期（{date}）",
		en: "📅 Expiry reminder\n{name} expires today ({date})",
	},

	// reason 与 cmd 不翻译：一个是拦截规则给出的原因，一个是用户/AI 敲的原文。
	// 与错误码里 Detail 的处理同理——那是证据，翻译反而会改变它。
	evtCommandBlocked: {
		zh: "🚫 命令被拦截\n\n机器：{name}\n调用方：{caller}\n原因：{reason}\n\n命令：\n{cmd}",
		en: "🚫 Command blocked\n\nServer: {name}\nCaller: {caller}\nReason: {reason}\n\nCommand:\n{cmd}",
	},

	evtGCPStarting: {
		zh: "🔄 GCP 自动开机\n{name} 已调用 instances.start（第 {tries}/{max} 次），等待节点上线",
		en: "🔄 GCP auto-start\n{name} called instances.start (attempt {tries}/{max}), waiting for the node to reconnect",
	},
	evtGCPFailed: {
		zh: "⚠️ GCP 自动开机失败\n{name} 第 {tries}/{max} 次：{err}",
		en: "⚠️ GCP auto-start failed\n{name} attempt {tries}/{max}: {err}",
	},
	txtGCPFailedStatus: {
		zh: "⚠️ GCP 自动开机失败\n{name} 第 {tries}/{max} 次：查询实例状态失败：{err}",
		en: "⚠️ GCP auto-start failed\n{name} attempt {tries}/{max}: could not query the instance state: {err}",
	},
	evtGCPGaveUp: {
		zh: "🛑 GCP 自动开机已停止\n{name} 已尝试 {max} 次仍未上线，等待人工处理（节点上线后自动复位）",
		en: "🛑 GCP auto-start halted\n{name} is still offline. Attempts made: {max}. Waiting for manual action — it resets once the node reconnects.",
	},
	evtGCPRunningNC: {
		zh: "⚠️ GCP 守护提醒\n{name} 实例状态为 RUNNING 但节点离线，可能是 agent 或网络故障，不执行开机",
		en: "⚠️ GCP guard notice\n{name}: the instance is RUNNING but the node is offline. Likely an agent or network fault, so no start was issued.",
	},
}

// metricNames 指标码 → 人话，供文案里的 {metric} 取用。
//
// metricNet 不在表里：网速的文案没有 {metric}，上/下行两个数字自带语义，
// 硬塞一个「网速 使用率」反而别扭。
var metricNames = map[string]alertText{
	metricCPU:  {zh: "CPU", en: "CPU"},
	metricMem:  {zh: "内存", en: "Memory"},
	metricDisk: {zh: "硬盘", en: "Disk"},
}

// renderAlert 按语言渲染一条告警的人类可读文案。
func renderAlert(ev alertEvent, lang string) string {
	key := ev.textKey
	if key == "" {
		key = ev.Type
	}
	tpl, ok := alertTexts[key]
	if !ok {
		// 未登记的类型（如 webhook 连通性测试）用产生处给的文案。
		// 两者都没有就退回事件类型——难看，但接收端至少知道发生了什么，
		// 而一条空消息 Telegram 根本发不出去，只会在日志里留个莫名其妙的 400。
		// 正常情况走不到这里：TestAlertTextsCoverAllEventTypes 盯着。
		if ev.Text != "" {
			return ev.Text
		}
		return key
	}
	s := tpl.pick(lang)

	pairs := make([]string, 0, len(ev.params)*2+2)
	for k, v := range ev.params {
		pairs = append(pairs, "{"+k+"}", v)
	}
	// {metric} 由 Metric 码现查，不让产生处塞人话进 params——
	// 那等于又把语言相关的东西带回了告警产生处。
	if name, ok := metricNames[ev.Metric]; ok {
		pairs = append(pairs, "{metric}", name.pick(lang))
	}
	if len(pairs) == 0 {
		return s
	}
	return strings.NewReplacer(pairs...).Replace(s)
}
