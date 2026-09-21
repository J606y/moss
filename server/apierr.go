package main

import (
	"errors"
	"net/http"
)

/*
错误码：让界面能把后端错误翻成访客的语言。

后端不关心访客在看哪种语言，只回答「是哪一种错」。翻译全在前端：
认得 code 就查表，不认得就退回显示 error 里的中文原文——所以任何时候
漏掉一条码，界面也只是显示中文，不会变成空白或 undefined。

码是语言中立的字符串而不是数字：日志里 `code=auth.bad_credentials`
自己就说清楚了，`code=7` 还得翻代码；而且新增时不会撞号，删掉一条
也不会在号段里留个没人敢用的洞。分段前缀（auth. / gcp. / upgrade.）
一眼看出归谁管。

	{ "error": "用户名或密码错误", "code": "auth.bad_credentials" }
*/

// codedError 是带错误码的底层错误。
//
// Error() 仍返回中文原文，所以日志、MCP 给 AI 的返回、审计记录里的文本
// 全都保持原样——这些消费者不需要、也不该被错误码改造波及。
// 出口处用 errors.As 把码挖出来，经 fmt.Errorf("%w") 包装过也挖得到。
type codedError struct {
	Code string
	Msg  string
	// Detail 同 apiErr.Detail：不翻译的那截（版本号、机器名、原始原因）。
	// 一律放末尾、Msg 以冒号收尾，这样中文兜底串直接拼接即可，
	// 不必在 Msg 里留 %s 占位——漏填一次就会把 %!s(MISSING) 发到界面上。
	Detail string
}

func (e codedError) Error() string { return e.Msg + e.Detail }

// with 附上不翻译的细节，返回副本。
func (e codedError) with(detail string) codedError {
	e.Detail = detail
	return e
}

// apiErr 是一条 HTTP 错误的完整描述。
//
// Detail 放不翻译的部分——机器名、Service Account 邮箱、Go error 原文之类。
// 前端文案里用 {detail} 接住它：中文「凭证无效: {detail}」、英文
// "Invalid credential: {detail}"，两边结构可以不同，细节本身不动。
type apiErr struct {
	Status int
	Code   string
	Msg    string
	Detail string
}

func (e apiErr) Error() string { return e.Msg + e.Detail }

// with 附上不翻译的细节。返回副本，错误表里的值不会被改到。
func (e apiErr) with(detail string) apiErr {
	e.Detail = detail
	return e
}

// as 换一个状态码。同一种错在不同出口偶尔要用不同状态码（如凭证冲突
// 在创建时是 409、在校验时是 400），码和文案不必因此分裂成两条。
func (e apiErr) as(status int) apiErr {
	e.Status = status
	return e
}

// 出口错误表。
//
// 集中定义而不是散在调用处，理由与 settings_keys.go 相同：码和文案绑在一起，
// 不会出现「改了文案忘了改码」；新增一条必须先在这里登记，也就不会有人
// 随手 writeErr 一句中文把界面又拉回不可翻译的状态。
//
// 带 {detail} 的条目文案以冒号结尾，调用处用 .with() 补上机器名、
// Service Account 邮箱或 Go error 原文——那部分不翻译，前端原样接住。
var (
	errInternal      = apiErr{Status: 500, Code: "internal", Msg: "内部错误"}
	errBadJSON       = apiErr{Status: 400, Code: "bad_json", Msg: "请求格式错误"}
	errBadParams     = apiErr{Status: 400, Code: "bad_params", Msg: "参数错误"}
	errMissingParams = apiErr{Status: 400, Code: "missing_params", Msg: "参数不完整"}
	errRateLimited   = apiErr{Status: 429, Code: "rate_limited", Msg: "请求过于频繁，请稍后再试"}

	errServerNotFound = apiErr{Status: 404, Code: "server.not_found", Msg: "服务器不存在"}
	errRecordNotFound = apiErr{Status: 404, Code: "audit.not_found", Msg: "记录不存在"}
	errNameRequired   = apiErr{Status: 400, Code: "name_required", Msg: "名称不能为空"}

	errBadCredentials   = apiErr{Status: 401, Code: "auth.bad_credentials", Msg: "用户名或密码错误"}
	errLockedOut        = apiErr{Status: 429, Code: "auth.locked_out", Msg: "登录失败次数过多，该 IP 已被锁定 30 分钟"}
	errUnauthorized     = apiErr{Status: 401, Code: "auth.unauthorized", Msg: "未登录"}
	errWrongPassword    = apiErr{Status: 401, Code: "auth.wrong_password", Msg: "当前密码错误"}
	errPasswordTooShort = apiErr{Status: 400, Code: "auth.password_too_short", Msg: "新密码至少 6 位"}

	errKeyNotFound   = apiErr{Status: 404, Code: "key.not_found", Msg: "密钥不存在"}
	errCapsRequired  = apiErr{Status: 400, Code: "key.caps_required", Msg: "至少需要选择一项能力"}
	errKeyBadServers = apiErr{Status: 400, Code: "key.unknown_server", Msg: "机器白名单包含不存在的服务器："}

	errWebhookURLRequired   = apiErr{Status: 400, Code: "webhook.url_required", Msg: "启用 webhook 需要填写地址"}
	errWebhookBadScheme     = apiErr{Status: 400, Code: "webhook.bad_scheme", Msg: "地址需以 http:// 或 https:// 开头"}
	errWebhookNotConfigured = apiErr{Status: 400, Code: "webhook.not_configured", Msg: "请先填写并保存 webhook 地址"}
	errWebhookEncrypt       = apiErr{Status: 500, Code: "webhook.encrypt_failed", Msg: "密钥加密失败，未保存，请检查服务器状态后重试"}

	errNotifyEncrypt       = apiErr{Status: 500, Code: "notify.encrypt_failed", Msg: "Bot Token 加密失败，未保存，请检查服务器状态后重试"}
	errNotifyNotConfigured = apiErr{Status: 400, Code: "notify.not_configured", Msg: "请先填写并保存 Bot Token 与 Chat ID"}
	errNotifySendFailed    = apiErr{Status: 502, Code: "notify.send_failed", Msg: "发送失败："}

	errCredNotFound    = apiErr{Status: 404, Code: "gcp.cred_not_found", Msg: "凭证不存在"}
	errGCPJSONRequired = apiErr{Status: 400, Code: "gcp.json_required", Msg: "请粘贴 Service Account JSON 密钥"}
	errGCPCredInvalid  = apiErr{Status: 400, Code: "gcp.cred_invalid", Msg: "凭证无效："}
	errGCPCredEmpty    = apiErr{Status: 400, Code: "gcp.cred_empty", Msg: "凭证内容为空，请删除后重新添加"}
	errGCPEncrypt      = apiErr{Status: 500, Code: "gcp.encrypt_failed", Msg: "凭证加密失败，未保存，请检查服务器状态后重试"}
	errGCPTokenFailed  = apiErr{Status: 502, Code: "gcp.token_failed", Msg: "换取访问令牌失败："}
	errGCPNotEnabled   = apiErr{Status: 400, Code: "gcp.not_enabled", Msg: "该服务器未启用 GCP 自动开机或未填写 zone/实例名"}
	errGCPZoneRequired = apiErr{Status: 400, Code: "gcp.zone_required", Msg: "GCP 自动开机需填写 zone 与实例名"}
	errGCPNoCred       = apiErr{Status: 400, Code: "gcp.no_credential", Msg: "尚未添加 GCP 凭证，请先到「GCP 守护」页添加"}
	errGCPPickCred     = apiErr{Status: 400, Code: "gcp.pick_credential", Msg: "请选择这台节点使用哪一份 GCP 凭证"}
	errGCPCredGone     = apiErr{Status: 400, Code: "gcp.cred_gone", Msg: "所选 GCP 凭证不存在，请刷新页面后重新选择"}
	// 解不开的凭证：文案里那条恢复路径是用户唯一的自救办法，
	// 英文界面下看不懂就只会点删除——而删掉就永久失去了。
	errGCPCredUndecryptable = apiErr{Status: 500, Code: "gcp.cred_undecryptable",
		Msg: "该凭证无法解密，主密钥可能已变更；请恢复原 MOSS_SECRET_KEY 或 secret.key，或删除后重新添加"}
	errGCPCredDuplicate = apiErr{Status: 409, Code: "gcp.cred_duplicate",
		Msg: "该 Service Account 已在凭证列表中；如需更换密钥，请先删除旧凭证再添加："}
	errGCPCredInUse = apiErr{Status: 409, Code: "gcp.cred_in_use",
		Msg: "该凭证正在被节点使用，请先把它们改绑到其他凭证，或关闭这些节点的 GCP 自动开机："}
	errGCPLegacyEndpoint = apiErr{Status: 400, Code: "gcp.legacy_endpoint",
		Msg: "凭证管理已迁移至 /api/admin/gcp/credentials（面板支持多份凭证），请刷新页面后重试"}

	errPanelHostNotFound = apiErr{Status: 400, Code: "panel.host_not_found", Msg: "选择的服务器不存在"}
	errPanelBusy         = apiErr{Status: 400, Code: "panel.already_running", Msg: "已有更新正在进行"}
	errPanelVersionCheck = apiErr{Status: 400, Code: "panel.version_check_failed", Msg: "版本查询失败："}
	errPanelDispatch     = apiErr{Status: 400, Code: "panel.dispatch_failed", Msg: "下发更新脚本失败："}
	errPanelStart        = apiErr{Status: 400, Code: "panel.start_failed", Msg: "启动更新失败："}
	errPanelNotNewer     = apiErr{Status: 400, Code: "panel.not_newer", Msg: "只能更新到比当前更高的版本，当前 "}
	errPanelHostUnset    = apiErr{Status: 400, Code: "panel.host_unset", Msg: "尚未指定面板所在的服务器"}
	errPanelHostMissing  = apiErr{Status: 400, Code: "panel.host_missing", Msg: "指定的服务器不存在，请重新选择"}
	// 机器名一律放末尾走 Detail：放在句中就得给 Msg 留 %s 占位，
	// 漏调一次 with() 就会把 %!s(MISSING) 发到界面上。
	errPanelHostOffline  = apiErr{Status: 400, Code: "panel.host_offline", Msg: "面板所在服务器当前离线："}
	errPanelHostOldAgent = apiErr{Status: 400, Code: "panel.host_old_agent", Msg: "面板所在服务器的 agent 版本过旧，请先手动升级："}
)

func writeErr(w http.ResponseWriter, e apiErr) {
	body := map[string]string{"error": e.Msg + e.Detail}
	// 码为空时不下发该字段：前端据此直接显示 error 原文，
	// 与改造前的行为完全一致。
	if e.Code != "" {
		body["code"] = e.Code
	}
	if e.Detail != "" {
		body["detail"] = e.Detail
	}
	writeJSON(w, e.Status, body)
}

// writeErrFrom 从一个 error 里挖出错误码回给前端，挖不到就用 fallback 的码、
// 但保留 err 自己的文案——那多半是条动态拼出来的消息，比笼统的兜底文案有用。
func writeErrFrom(w http.ResponseWriter, status int, err error, fallback apiErr) {
	var ce codedError
	if errors.As(err, &ce) {
		writeErr(w, apiErr{Status: status, Code: ce.Code, Msg: ce.Msg, Detail: ce.Detail})
		return
	}
	// 没挂码的 error：连中文原文一起回给前端，界面照旧显示它。
	// 不翻译，但也不会变成空白——这正是「漏一条只是不翻译」的兜底。
	writeErr(w, apiErr{Status: status, Code: fallback.Code, Msg: err.Error()})
}
