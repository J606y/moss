package main

import (
	"database/sql"
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
	UUID      string `json:"uuid,omitempty"`
	Name      string `json:"name"`
	Group     string `json:"group"`
	Region    string `json:"region"`
	Flag      string `json:"flag"`
	AutoFlag  string `json:"autoFlag"`
	Note      string `json:"note"`
	ExpireAt  string `json:"expireAt"`
	Token     string `json:"token"`
	IP        string `json:"ip"`
	IPv6      string `json:"ipv6"`
	Online    bool   `json:"online"`
	LastSeen  int64  `json:"lastSeen"`
	CreatedAt int64  `json:"createdAt"`

	AgentVersion      string `json:"agentVersion"`
	TargetVersion     string `json:"targetVersion"`
	Upgradable        bool   `json:"upgradable"`
	UpgradeHint       string `json:"upgradeHint,omitempty"`
	UpgradeHintCode   string `json:"upgradeHintCode,omitempty"`
	UpgradeHintDetail string `json:"upgradeHintDetail,omitempty"`
	UpgradeStage      string `json:"upgradeStage,omitempty"`
	UpgradeErr        string `json:"upgradeErr,omitempty"`

	GcpEnabled  bool   `json:"gcpEnabled"`
	GcpCredID   string `json:"gcpCredId"`
	GcpProject  string `json:"gcpProject"`
	GcpZone     string `json:"gcpZone"`
	GcpInstance string `json:"gcpInstance"`
	GcpTries    int    `json:"gcpTries"`
	GcpLastTry  int64  `json:"gcpLastTry"`
	GcpLastErr  string `json:"gcpLastErr"`
}

type serverForm struct {
	ID          string `json:"id"`
	UUID        string `json:"uuid"`
	Name        string `json:"name"`
	Group       string `json:"group"`
	Region      string `json:"region"`
	Flag        string `json:"flag"`
	Note        string `json:"note"`
	ExpireAt    string `json:"expireAt"`
	GcpEnabled  bool   `json:"gcpEnabled"`
	GcpCredID   string `json:"gcpCredId"`
	GcpProject  string `json:"gcpProject"`
	GcpZone     string `json:"gcpZone"`
	GcpInstance string `json:"gcpInstance"`
}

func (s *App) handleAdminServers(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(
		`SELECT id, uuid, name, grp, region, flag, auto_flag, note, expire_at, token, ip, ipv6, last_seen, created_at,
		        gcp_enabled, gcp_cred_id, gcp_project, gcp_zone, gcp_instance, agent_version, os
		 FROM servers ORDER BY sort, created_at`)
	if err != nil {
		log.Printf("handleAdminServers query: %v", err)
		writeErr(w, errInternal)
		return
	}
	defer rows.Close()
	out := []adminServer{}
	for rows.Next() {
		var a adminServer
		var agentOS string
		if err := rows.Scan(&a.ID, &a.UUID, &a.Name, &a.Group, &a.Region, &a.Flag, &a.AutoFlag, &a.Note,
			&a.ExpireAt, &a.Token, &a.IP, &a.IPv6, &a.LastSeen, &a.CreatedAt,
			&a.GcpEnabled, &a.GcpCredID, &a.GcpProject, &a.GcpZone, &a.GcpInstance, &a.AgentVersion, &agentOS); err != nil {
			log.Printf("handleAdminServers scan: %v", err)
			writeErr(w, errInternal)
			return
		}
		_, _, a.Online = s.hub.Snapshot(a.ID)
		a.GcpTries, a.GcpLastTry, a.GcpLastErr = s.notifier.GCPStatus(a.ID)
		a.TargetVersion = upgradeTarget()
		var hint *codedError
		a.Upgradable, hint = upgradeAvailability(a.AgentVersion, agentOS, a.Online)
		if hint != nil {
			a.UpgradeHint, a.UpgradeHintCode, a.UpgradeHintDetail = hint.Error(), hint.Code, hint.Detail
		}
		a.UpgradeStage, a.UpgradeErr = s.upgrade.Status(a.ID)
		out = append(out, a)
	}
	writeJSON(w, 200, out)
}

func normalizeGCP(db *sql.DB, f *serverForm) *apiErr {
	f.GcpCredID = strings.TrimSpace(f.GcpCredID)
	f.GcpProject = strings.TrimSpace(f.GcpProject)
	f.GcpZone = strings.TrimSpace(f.GcpZone)
	f.GcpInstance = strings.TrimSpace(f.GcpInstance)
	if !f.GcpEnabled {
		return nil
	}
	if f.GcpZone == "" || f.GcpInstance == "" {
		return &errGCPZoneRequired
	}
	if f.GcpCredID == "" {
		if only := gcpOnlyCredentialID(db); only != "" {
			f.GcpCredID = only
			return nil
		}
		if n, err := countGCPCredentials(db); err == nil && n == 0 {
			return &errGCPNoCred
		}
		return &errGCPPickCred
	}
	var got string
	if err := db.QueryRow(`SELECT id FROM gcp_credentials WHERE id = ?`, f.GcpCredID).Scan(&got); err != nil {
		return &errGCPCredGone
	}
	return nil
}

func isValidServerID(s string) bool {
	if len(s) < 2 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_'
		if !ok {
			return false
		}
	}
	return true
}

// handleAddServer 新增或重注册节点。
//
// 支持固定节点 UUID 与固定 ID：
//   - 若传入的 uuid（或显式指定的 id）在库中已存在对应节点，则直接复用原 server_id 与 token（或更新配置），
//     避免节点重装/重注册后 server_id 漂移导致白名单、历史曲线与审计断链；
//   - 若传入新的合法固定 id，则按指定 id 建档，否则生成 8 位随机 id。
func (s *App) handleAddServer(w http.ResponseWriter, r *http.Request) {
	var f serverForm
	if err := json.NewDecoder(r.Body).Decode(&f); err != nil || f.Name == "" {
		writeErr(w, errNameRequired)
		return
	}
	if f.Group == "" {
		f.Group = "默认"
	}
	if e := normalizeGCP(s.db, &f); e != nil {
		writeErr(w, *e)
		return
	}
	f.ID = strings.TrimSpace(f.ID)
	f.UUID = strings.ToLower(strings.TrimSpace(f.UUID))

	var existingID, existingToken string
	if f.UUID != "" {
		_ = s.db.QueryRow(`SELECT id, token FROM servers WHERE uuid = ? LIMIT 1`, f.UUID).Scan(&existingID, &existingToken)
	}
	if existingID == "" && f.ID != "" {
		_ = s.db.QueryRow(`SELECT id, token FROM servers WHERE id = ? LIMIT 1`, f.ID).Scan(&existingID, &existingToken)
	}
	if existingID != "" {
		if _, err := s.db.Exec(
			`UPDATE servers SET name=?, grp=?, region=?, flag=?, note=?, expire_at=?,
			        uuid=COALESCE(NULLIF(?, ''), uuid),
			        gcp_enabled=?, gcp_cred_id=?, gcp_project=?, gcp_zone=?, gcp_instance=?
			  WHERE id=?`,
			f.Name, f.Group, f.Region, f.Flag, f.Note, f.ExpireAt,
			f.UUID,
			f.GcpEnabled, f.GcpCredID, f.GcpProject, f.GcpZone, f.GcpInstance,
			existingID,
		); err != nil {
			log.Printf("handleAddServer reuse update (id=%s): %v", existingID, err)
			writeErr(w, errInternal)
			return
		}
		s.hub.BroadcastMeta()
		writeJSON(w, 200, map[string]any{"id": existingID, "uuid": f.UUID, "token": existingToken, "reused": true})
		return
	}

	id := randString(8)
	if f.ID != "" && isValidServerID(f.ID) {
		id = f.ID
	}
	token := newToken()
	var maxSort int
	s.db.QueryRow(`SELECT COALESCE(MAX(sort), 0) FROM servers`).Scan(&maxSort)
	if _, err := s.db.Exec(
		`INSERT INTO servers(id, uuid, token, name, grp, region, flag, note, expire_at, sort, created_at,
		                     gcp_enabled, gcp_cred_id, gcp_project, gcp_zone, gcp_instance)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, f.UUID, token, f.Name, f.Group, f.Region, f.Flag, f.Note, f.ExpireAt, maxSort+1, time.Now().Unix(),
		f.GcpEnabled, f.GcpCredID, f.GcpProject, f.GcpZone, f.GcpInstance,
	); err != nil {
		log.Printf("handleAddServer insert (id=%s): %v", id, err)
		writeErr(w, errInternal)
		return
	}
	s.hub.BroadcastMeta()
	writeJSON(w, 200, map[string]string{"id": id, "uuid": f.UUID, "token": token})
}

func (s *App) handleUpdateServer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var f serverForm
	if err := json.NewDecoder(r.Body).Decode(&f); err != nil || f.Name == "" {
		writeErr(w, errNameRequired)
		return
	}
	if f.Group == "" {
		f.Group = "默认"
	}
	if e := normalizeGCP(s.db, &f); e != nil {
		writeErr(w, *e)
		return
	}
	var old serverForm
	s.db.QueryRow(
		`SELECT gcp_enabled, gcp_cred_id, gcp_project, gcp_zone, gcp_instance FROM servers WHERE id=?`, id).
		Scan(&old.GcpEnabled, &old.GcpCredID, &old.GcpProject, &old.GcpZone, &old.GcpInstance)
	res, err := s.db.Exec(
		`UPDATE servers SET name=?, grp=?, region=?, flag=?, note=?, expire_at=?,
		        gcp_enabled=?, gcp_cred_id=?, gcp_project=?, gcp_zone=?, gcp_instance=? WHERE id=?`,
		f.Name, f.Group, f.Region, f.Flag, f.Note, f.ExpireAt,
		f.GcpEnabled, f.GcpCredID, f.GcpProject, f.GcpZone, f.GcpInstance, id)
	if err != nil {
		log.Printf("handleUpdateServer (id=%s): %v", id, err)
		writeErr(w, errInternal)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeErr(w, errServerNotFound)
		return
	}
	if old.GcpEnabled != f.GcpEnabled || old.GcpCredID != f.GcpCredID || old.GcpProject != f.GcpProject ||
		old.GcpZone != f.GcpZone || old.GcpInstance != f.GcpInstance {
		s.notifier.ResetGCPState(id)
	}
	s.hub.BroadcastMeta()
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *App) handleDeleteServer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.db.Exec(`DELETE FROM servers WHERE id=?`, id); err != nil {
		log.Printf("handleDeleteServer (id=%s): %v", id, err)
		writeErr(w, errInternal)
		return
	}
	s.db.Exec(`DELETE FROM history WHERE server_id=?`, id)
	s.db.Exec(`DELETE FROM ping_results WHERE server_id=?`, id)
	s.notifier.ResetGCPState(id)
	s.hub.Drop(id)
	s.hub.BroadcastMeta()
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *App) handleReorderServers(w http.ResponseWriter, r *http.Request) {
	var ids []string
	if err := json.NewDecoder(r.Body).Decode(&ids); err != nil {
		writeErr(w, errBadJSON)
		return
	}
	tx, err := s.db.Begin()
	if err != nil {
		log.Printf("handleReorderServers begin: %v", err)
		writeErr(w, errInternal)
		return
	}
	defer tx.Rollback()
	for i, id := range ids {
		if _, err := tx.Exec(`UPDATE servers SET sort=? WHERE id=?`, i, id); err != nil {
			log.Printf("handleReorderServers update (id=%s): %v", id, err)
			writeErr(w, errInternal)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		log.Printf("handleReorderServers commit: %v", err)
		writeErr(w, errInternal)
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
	ServerID string `json:"serverId"`
}

func validTaskType(t string) bool { return t == "icmp" || t == "tcp" || t == "http" }

func (s *App) handleAdminTasks(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(`SELECT id, name, type, target, interval, enabled, server_id FROM ping_tasks ORDER BY sort, id`)
	if err != nil {
		log.Printf("handleAdminTasks query: %v", err)
		writeErr(w, errInternal)
		return
	}
	defer rows.Close()
	out := []taskItem{}
	for rows.Next() {
		var t taskItem
		if err := rows.Scan(&t.ID, &t.Name, &t.Type, &t.Target, &t.Interval, &t.Enabled, &t.ServerID); err != nil {
			log.Printf("handleAdminTasks scan: %v", err)
			writeErr(w, errInternal)
			return
		}
		out = append(out, t)
	}
	writeJSON(w, 200, out)
}

func (s *App) handleAddTask(w http.ResponseWriter, r *http.Request) {
	var t taskItem
	if err := json.NewDecoder(r.Body).Decode(&t); err != nil || t.Name == "" || t.Target == "" || !validTaskType(t.Type) {
		writeErr(w, errMissingParams)
		return
	}
	if t.Interval < 10 {
		t.Interval = 60
	}
	var maxSort int
	s.db.QueryRow(`SELECT COALESCE(MAX(sort), 0) FROM ping_tasks`).Scan(&maxSort)
	res, err := s.db.Exec(
		`INSERT INTO ping_tasks(name, type, target, interval, enabled, server_id, sort) VALUES(?, ?, ?, ?, ?, ?, ?)`,
		t.Name, t.Type, t.Target, t.Interval, t.Enabled, t.ServerID, maxSort+1)
	if err != nil {
		log.Printf("handleAddTask insert: %v", err)
		writeErr(w, errInternal)
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
		writeErr(w, errMissingParams)
		return
	}
	if t.Interval < 10 {
		t.Interval = 60
	}
	if _, err := s.db.Exec(
		`UPDATE ping_tasks SET name=?, type=?, target=?, interval=?, enabled=?, server_id=? WHERE id=?`,
		t.Name, t.Type, t.Target, t.Interval, t.Enabled, t.ServerID, id); err != nil {
		log.Printf("handleUpdateTask (id=%d): %v", id, err)
		writeErr(w, errInternal)
		return
	}
	s.pushConfigAll()
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *App) handleDeleteTask(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if _, err := s.db.Exec(`DELETE FROM ping_tasks WHERE id=?`, id); err != nil {
		log.Printf("handleDeleteTask (id=%d): %v", id, err)
		writeErr(w, errInternal)
		return
	}
	s.db.Exec(`DELETE FROM ping_results WHERE task_id=?`, id)
	s.pushConfigAll()
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *App) handleReorderTasks(w http.ResponseWriter, r *http.Request) {
	var ids []int64
	if err := json.NewDecoder(r.Body).Decode(&ids); err != nil {
		writeErr(w, errBadJSON)
		return
	}
	tx, err := s.db.Begin()
	if err != nil {
		log.Printf("handleReorderTasks begin: %v", err)
		writeErr(w, errInternal)
		return
	}
	defer tx.Rollback()
	for i, id := range ids {
		if _, err := tx.Exec(`UPDATE ping_tasks SET sort=? WHERE id=?`, i, id); err != nil {
			log.Printf("handleReorderTasks update (id=%d): %v", id, err)
			writeErr(w, errInternal)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		log.Printf("handleReorderTasks commit: %v", err)
		writeErr(w, errInternal)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

/* ---------- 站点设置 ---------- */

type settingsView struct {
	Username         string `json:"username"`
	SiteName         string `json:"siteName"`
	SiteDesc         string `json:"siteDesc"`
	Lang             string `json:"lang"`
	ReportInterval   int    `json:"reportInterval"`
	SampleInterval   int    `json:"sampleInterval"`
	HistoryDays      int    `json:"historyDays"`
	PingDays         int    `json:"pingDays"`
	ExecAuditDays    int    `json:"execAuditDays"`
	ExecAuditMaxRows int    `json:"execAuditMaxRows"`
}

func (s *App) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, settingsView{
		Username:         getSetting(s.db, keyUsername, "admin"),
		SiteName:         getSetting(s.db, keySiteName, "Moss"),
		SiteDesc:         getSetting(s.db, keySiteDesc, "智控中心"),
		Lang:             normLang(getSetting(s.db, keyLang, "auto")),
		ReportInterval:   getSettingInt(s.db, keyReportInterval, 2),
		SampleInterval:   getSettingInt(s.db, keySampleInterval, 10),
		HistoryDays:      getSettingInt(s.db, keyHistoryDays, 7),
		PingDays:         getSettingInt(s.db, keyPingDays, 7),
		ExecAuditDays:    getSettingInt(s.db, keyExecAuditDays, 90),
		ExecAuditMaxRows: getSettingInt(s.db, keyExecAuditMaxRows, execAuditMaxRows),
	})
}

func clampInt(v, min, max, fallback int) int {
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
		writeErr(w, errBadJSON)
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
	setSetting(s.db, keyLang, normLang(v.Lang))
	setSetting(s.db, keyReportInterval, strconv.Itoa(clampInt(v.ReportInterval, 1, 60, 2)))
	setSetting(s.db, keySampleInterval, strconv.Itoa(clampInt(v.SampleInterval, 5, 3600, 10)))
	setSetting(s.db, keyHistoryDays, strconv.Itoa(clampInt(v.HistoryDays, 1, 365, 7)))
	setSetting(s.db, keyPingDays, strconv.Itoa(clampInt(v.PingDays, 1, 90, 7)))
	setSetting(s.db, keyExecAuditDays, strconv.Itoa(clampInt(v.ExecAuditDays, 7, 90, 90)))
	setSetting(s.db, keyExecAuditMaxRows,
		strconv.Itoa(clampInt(v.ExecAuditMaxRows, execAuditMinRows, execAuditMaxRows, execAuditMaxRows)))
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
		writeErr(w, errPasswordTooShort)
		return
	}
	if !s.checkPassword(body.Old) {
		writeErr(w, errWrongPassword)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(body.New), bcrypt.DefaultCost)
	if err != nil {
		log.Printf("handleChangePassword bcrypt: %v", err)
		writeErr(w, errInternal)
		return
	}
	setSetting(s.db, keyPasswordHash, string(hash))
	s.db.Exec(`DELETE FROM sessions`)
	writeJSON(w, 200, map[string]bool{"ok": true})
}
