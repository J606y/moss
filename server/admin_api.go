package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

/* ---------- 服务器管理 ---------- */

type adminServer struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Group     string `json:"group"`
	Region    string `json:"region"`
	Flag      string `json:"flag"`     // 手动设置的国旗（编辑表单用原值）
	AutoFlag  string `json:"autoFlag"` // agent 自动识别的国旗（列表回退显示用）
	Note      string `json:"note"`
	ExpireAt  string `json:"expireAt"`
	Token     string `json:"token"`
	IP        string `json:"ip"`
	IPv6      string `json:"ipv6"`
	Online    bool   `json:"online"`
	LastSeen  int64  `json:"lastSeen"`
	CreatedAt int64  `json:"createdAt"`

	// GCP Spot 自动开机配置与运行态（运行态为内存值，面板重启归零）
	GcpEnabled  bool   `json:"gcpEnabled"`
	GcpProject  string `json:"gcpProject"`
	GcpZone     string `json:"gcpZone"`
	GcpInstance string `json:"gcpInstance"`
	GcpTries    int    `json:"gcpTries"`
	GcpLastTry  int64  `json:"gcpLastTry"`
	GcpLastErr  string `json:"gcpLastErr"`
}

type serverForm struct {
	Name        string `json:"name"`
	Group       string `json:"group"`
	Region      string `json:"region"`
	Flag        string `json:"flag"`
	Note        string `json:"note"`
	ExpireAt    string `json:"expireAt"`
	GcpEnabled  bool   `json:"gcpEnabled"`
	GcpProject  string `json:"gcpProject"` // 留空 = 用 SA JSON 的 project_id
	GcpZone     string `json:"gcpZone"`
	GcpInstance string `json:"gcpInstance"`
}

func (s *App) handleAdminServers(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(
		`SELECT id, name, grp, region, flag, auto_flag, note, expire_at, token, ip, ipv6, last_seen, created_at,
		        gcp_enabled, gcp_project, gcp_zone, gcp_instance
		 FROM servers ORDER BY sort, created_at`)
	if err != nil {
		log.Printf("handleAdminServers query: %v", err)
		writeErr(w, 500, "内部错误")
		return
	}
	defer rows.Close()
	out := []adminServer{}
	for rows.Next() {
		var a adminServer
		if err := rows.Scan(&a.ID, &a.Name, &a.Group, &a.Region, &a.Flag, &a.AutoFlag, &a.Note,
			&a.ExpireAt, &a.Token, &a.IP, &a.IPv6, &a.LastSeen, &a.CreatedAt,
			&a.GcpEnabled, &a.GcpProject, &a.GcpZone, &a.GcpInstance); err != nil {
			log.Printf("handleAdminServers scan: %v", err)
			writeErr(w, 500, "内部错误")
			return
		}
		_, _, a.Online = s.hub.Snapshot(a.ID)
		a.GcpTries, a.GcpLastTry, a.GcpLastErr = s.notifier.GCPStatus(a.ID)
		out = append(out, a)
	}
	writeJSON(w, 200, out)
}

// normalizeGCP 整理并校验表单里的 GCP 字段（启用时 zone/实例名必填），返回错误提示，空串为通过。
func normalizeGCP(f *serverForm) string {
	f.GcpProject = strings.TrimSpace(f.GcpProject)
	f.GcpZone = strings.TrimSpace(f.GcpZone)
	f.GcpInstance = strings.TrimSpace(f.GcpInstance)
	if f.GcpEnabled && (f.GcpZone == "" || f.GcpInstance == "") {
		return "GCP 自动开机需填写 zone 与实例名"
	}
	return ""
}

func (s *App) handleAddServer(w http.ResponseWriter, r *http.Request) {
	var f serverForm
	if err := json.NewDecoder(r.Body).Decode(&f); err != nil || f.Name == "" {
		writeErr(w, 400, "名称不能为空")
		return
	}
	if f.Group == "" {
		f.Group = "默认"
	}
	if msg := normalizeGCP(&f); msg != "" {
		writeErr(w, 400, msg)
		return
	}
	id := randString(8)
	token := newToken()
	// 新服务器排到列表末尾：sort = 当前最大值 + 1
	var maxSort int
	s.db.QueryRow(`SELECT COALESCE(MAX(sort), 0) FROM servers`).Scan(&maxSort)
	if _, err := s.db.Exec(
		`INSERT INTO servers(id, token, name, grp, region, flag, note, expire_at, sort, created_at,
		                     gcp_enabled, gcp_project, gcp_zone, gcp_instance)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, token, f.Name, f.Group, f.Region, f.Flag, f.Note, f.ExpireAt, maxSort+1, time.Now().Unix(),
		f.GcpEnabled, f.GcpProject, f.GcpZone, f.GcpInstance,
	); err != nil {
		log.Printf("handleAddServer insert (id=%s): %v", id, err)
		writeErr(w, 500, "内部错误")
		return
	}
	s.hub.BroadcastMeta()
	writeJSON(w, 200, map[string]string{"id": id, "token": token})
}

func (s *App) handleUpdateServer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var f serverForm
	if err := json.NewDecoder(r.Body).Decode(&f); err != nil || f.Name == "" {
		writeErr(w, 400, "名称不能为空")
		return
	}
	if f.Group == "" {
		f.Group = "默认"
	}
	if msg := normalizeGCP(&f); msg != "" {
		writeErr(w, 400, msg)
		return
	}
	res, err := s.db.Exec(
		`UPDATE servers SET name=?, grp=?, region=?, flag=?, note=?, expire_at=?,
		        gcp_enabled=?, gcp_project=?, gcp_zone=?, gcp_instance=? WHERE id=?`,
		f.Name, f.Group, f.Region, f.Flag, f.Note, f.ExpireAt,
		f.GcpEnabled, f.GcpProject, f.GcpZone, f.GcpInstance, id)
	if err != nil {
		log.Printf("handleUpdateServer (id=%s): %v", id, err)
		writeErr(w, 500, "内部错误")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeErr(w, 404, "服务器不存在")
		return
	}
	s.hub.BroadcastMeta()
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *App) handleDeleteServer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.db.Exec(`DELETE FROM servers WHERE id=?`, id); err != nil {
		log.Printf("handleDeleteServer (id=%s): %v", id, err)
		writeErr(w, 500, "内部错误")
		return
	}
	s.db.Exec(`DELETE FROM history WHERE server_id=?`, id)
	s.db.Exec(`DELETE FROM ping_results WHERE server_id=?`, id)
	s.hub.Drop(id)
	s.hub.BroadcastMeta()
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// handleReorderServers 按前端拖拽后的顺序重写 sort（数组下标即新顺序）。
func (s *App) handleReorderServers(w http.ResponseWriter, r *http.Request) {
	var ids []string
	if err := json.NewDecoder(r.Body).Decode(&ids); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	tx, err := s.db.Begin()
	if err != nil {
		log.Printf("handleReorderServers begin: %v", err)
		writeErr(w, 500, "内部错误")
		return
	}
	defer tx.Rollback()
	for i, id := range ids {
		if _, err := tx.Exec(`UPDATE servers SET sort=? WHERE id=?`, i, id); err != nil {
			log.Printf("handleReorderServers update (id=%s): %v", id, err)
			writeErr(w, 500, "内部错误")
			return
		}
	}
	if err := tx.Commit(); err != nil {
		log.Printf("handleReorderServers commit: %v", err)
		writeErr(w, 500, "内部错误")
		return
	}
	s.hub.BroadcastMeta()
	writeJSON(w, 200, map[string]bool{"ok": true})
}

/* ---------- 探测任务 ---------- */

type taskItem struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	Target   string `json:"target"`
	Interval int    `json:"interval"`
	Enabled  bool   `json:"enabled"`
	ServerID string `json:"serverId"` // '' = 全部服务器
}

func validTaskType(t string) bool { return t == "icmp" || t == "tcp" || t == "http" }

func (s *App) handleAdminTasks(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(`SELECT id, name, type, target, interval, enabled, server_id FROM ping_tasks ORDER BY sort, id`)
	if err != nil {
		log.Printf("handleAdminTasks query: %v", err)
		writeErr(w, 500, "内部错误")
		return
	}
	defer rows.Close()
	out := []taskItem{}
	for rows.Next() {
		var t taskItem
		if err := rows.Scan(&t.ID, &t.Name, &t.Type, &t.Target, &t.Interval, &t.Enabled, &t.ServerID); err != nil {
			log.Printf("handleAdminTasks scan: %v", err)
			writeErr(w, 500, "内部错误")
			return
		}
		out = append(out, t)
	}
	writeJSON(w, 200, out)
}

func (s *App) handleAddTask(w http.ResponseWriter, r *http.Request) {
	var t taskItem
	if err := json.NewDecoder(r.Body).Decode(&t); err != nil || t.Name == "" || t.Target == "" || !validTaskType(t.Type) {
		writeErr(w, 400, "参数不完整")
		return
	}
	if t.Interval < 10 {
		t.Interval = 60
	}
	// 新任务排到列表末尾：sort = 当前最大值 + 1
	var maxSort int
	s.db.QueryRow(`SELECT COALESCE(MAX(sort), 0) FROM ping_tasks`).Scan(&maxSort)
	res, err := s.db.Exec(
		`INSERT INTO ping_tasks(name, type, target, interval, enabled, server_id, sort) VALUES(?, ?, ?, ?, ?, ?, ?)`,
		t.Name, t.Type, t.Target, t.Interval, t.Enabled, t.ServerID, maxSort+1)
	if err != nil {
		log.Printf("handleAddTask insert: %v", err)
		writeErr(w, 500, "内部错误")
		return
	}
	id, _ := res.LastInsertId()
	s.pushConfigAll()
	writeJSON(w, 200, map[string]int64{"id": id})
}

func (s *App) handleUpdateTask(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var t taskItem
	if err := json.NewDecoder(r.Body).Decode(&t); err != nil || t.Name == "" || t.Target == "" || !validTaskType(t.Type) {
		writeErr(w, 400, "参数不完整")
		return
	}
	if t.Interval < 10 {
		t.Interval = 60
	}
	if _, err := s.db.Exec(
		`UPDATE ping_tasks SET name=?, type=?, target=?, interval=?, enabled=?, server_id=? WHERE id=?`,
		t.Name, t.Type, t.Target, t.Interval, t.Enabled, t.ServerID, id); err != nil {
		log.Printf("handleUpdateTask (id=%d): %v", id, err)
		writeErr(w, 500, "内部错误")
		return
	}
	s.pushConfigAll()
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *App) handleDeleteTask(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if _, err := s.db.Exec(`DELETE FROM ping_tasks WHERE id=?`, id); err != nil {
		log.Printf("handleDeleteTask (id=%d): %v", id, err)
		writeErr(w, 500, "内部错误")
		return
	}
	s.db.Exec(`DELETE FROM ping_results WHERE task_id=?`, id)
	s.pushConfigAll()
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// handleReorderTasks 按前端拖拽后的顺序重写 sort（数组下标即新顺序）。
// 顺序只影响展示（后台列表与详情页延迟图），无需向 Agent 重新下发配置。
func (s *App) handleReorderTasks(w http.ResponseWriter, r *http.Request) {
	var ids []int64
	if err := json.NewDecoder(r.Body).Decode(&ids); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	tx, err := s.db.Begin()
	if err != nil {
		log.Printf("handleReorderTasks begin: %v", err)
		writeErr(w, 500, "内部错误")
		return
	}
	defer tx.Rollback()
	for i, id := range ids {
		if _, err := tx.Exec(`UPDATE ping_tasks SET sort=? WHERE id=?`, i, id); err != nil {
			log.Printf("handleReorderTasks update (id=%d): %v", id, err)
			writeErr(w, 500, "内部错误")
			return
		}
	}
	if err := tx.Commit(); err != nil {
		log.Printf("handleReorderTasks commit: %v", err)
		writeErr(w, 500, "内部错误")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

/* ---------- 站点设置 ---------- */

type settingsView struct {
	Username       string `json:"username"`
	SiteName       string `json:"siteName"`
	SiteDesc       string `json:"siteDesc"`
	ReportInterval int    `json:"reportInterval"`
	SampleInterval int    `json:"sampleInterval"`
	HistoryDays    int    `json:"historyDays"`
	PingDays       int    `json:"pingDays"`
}

func (s *App) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, settingsView{
		Username:       getSetting(s.db, keyUsername, "admin"),
		SiteName:       getSetting(s.db, keySiteName, "Moss"),
		SiteDesc:       getSetting(s.db, keySiteDesc, "轻量服务器监控"),
		ReportInterval: getSettingInt(s.db, keyReportInterval, 2),
		SampleInterval: getSettingInt(s.db, keySampleInterval, 10),
		HistoryDays:    getSettingInt(s.db, keyHistoryDays, 7),
		PingDays:       getSettingInt(s.db, keyPingDays, 7),
	})
}

func clampInt(v, min, max, fallback int) int {
	// 注意：v==0 被视为「未设→回退 fallback」，因此 notify 的 CPU/内存/硬盘阈值
	// 无法用 0 单独禁用某一项告警，应使用 LoadOn 主开关整体关闭；
	// 若需逐项禁用，须把 API 改为指针/presence 判断（区分「显式 0」与「缺省」）。
	if v == 0 {
		return fallback
	}
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

func (s *App) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var v settingsView
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	if v.SiteName == "" {
		v.SiteName = "Moss"
	}
	if v.Username == "" {
		v.Username = "admin"
	}
	setSetting(s.db, keyUsername, v.Username)
	setSetting(s.db, keySiteName, v.SiteName)
	setSetting(s.db, keySiteDesc, v.SiteDesc)
	setSetting(s.db, keyReportInterval, strconv.Itoa(clampInt(v.ReportInterval, 1, 60, 2)))
	setSetting(s.db, keySampleInterval, strconv.Itoa(clampInt(v.SampleInterval, 5, 3600, 10)))
	setSetting(s.db, keyHistoryDays, strconv.Itoa(clampInt(v.HistoryDays, 1, 365, 7)))
	setSetting(s.db, keyPingDays, strconv.Itoa(clampInt(v.PingDays, 1, 90, 7)))
	s.pushConfigAll()
	s.hub.BroadcastMeta()
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *App) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Old string `json:"old"`
		New string `json:"new"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.New) < 6 {
		writeErr(w, 400, "新密码至少 6 位")
		return
	}
	if !s.checkPassword(body.Old) {
		writeErr(w, 401, "当前密码错误")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(body.New), bcrypt.DefaultCost)
	if err != nil {
		log.Printf("handleChangePassword bcrypt: %v", err)
		writeErr(w, 500, "内部错误")
		return
	}
	setSetting(s.db, keyPasswordHash, string(hash))
	// 修改密码后吊销所有会话
	s.db.Exec(`DELETE FROM sessions`)
	writeJSON(w, 200, map[string]bool{"ok": true})
}
