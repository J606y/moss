package main

// settings 表的 key 常量，集中定义避免写入端/读取端拼写漂移
// （拼错时 getSetting 会静默返回 fallback，难以排查）。
const (
	keySiteName       = "site_name"
	keySiteDesc       = "site_desc"
	keyReportInterval = "report_interval"
	keySampleInterval = "sample_interval"
	keySampleUnitSec  = "sample_unit_sec"
	keyHistoryDays    = "history_days"
	keyPingDays       = "ping_days"

	keyUsername     = "username"
	keyPasswordHash = "password_hash"

	keyNotifyTgToken      = "notify_tg_token"
	keyNotifyTgChat       = "notify_tg_chat"
	keyNotifyOffline      = "notify_offline"
	keyNotifyOfflineDelay = "notify_offline_delay"
	keyNotifyLoad         = "notify_load"
	keyNotifyCPU          = "notify_cpu"
	keyNotifyMem          = "notify_mem"
	keyNotifyDisk         = "notify_disk"
	keyNotifyLoadMin      = "notify_load_min"
	keyNotifyNet          = "notify_net"
	keyNotifyNetMB        = "notify_net_mb"
	keyNotifyNetSec       = "notify_net_sec"
	keyNotifyExpire       = "notify_expire"
	keyNotifyExpireDays   = "notify_expire_days"

	keyGCPSAJSON        = "gcp_sa_json"
	keyGCPAutoOn        = "gcp_auto_on"
	keyGCPStartDelay    = "gcp_start_delay"
	keyGCPStartCooldown = "gcp_start_cooldown"
	keyGCPStartMaxTries = "gcp_start_max_tries"
)
