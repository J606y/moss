package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"

	"golang.org/x/crypto/bcrypt"
)

/* ---------- 服务器管理 ---------- */

type adminServer struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Group     string `json:"group"`
	Region    string `json:"region"`
	Flag      string `json:"flag"`
	Note      string `json:"note"`
	ExpireAt  string `json:"expireAt"`
	Token     string `json:"token"`
	IP        string `json:"ip"`
	Online    bool   `json:"online"`
	LastSeen  int64  `json:"lastSeen"`
	CreatedAt int64  `json:"createdAt"`
}

type serverForm struct {
	Name     string `json:"name"`
	Group    string `json:"group"`
	Region   string `json:"region"`
	Flag     string `json:"flag"`
	Note     string `json:"note"`
	ExpireAt string `json:"expireAt"`
}

func (s *App) handleAdminServers(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(
		`SELECT id, name, grp, region, flag, note, expire_at, token, ip, last_seen, created_at
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
		if err := rows.Scan(&a.ID, &a.Name, &a.Group, &a.Region, &a.Flag, &a.Note,
			&a.ExpireAt, &a.Token, &a.IP, &a.LastSeen, &a.CreatedAt); err != nil {
			log.Printf("handleAdminServers scan: %v", err)
			writeErr(w, 500, "内部错误")
			return
		}
		_, _, a.Online = s.hub.Snapshot(a.ID)
		out = append(out, a)
	}
	writeJSON(w, 200, out)
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
	id := randString(8)
	token := newToken()
	if _, err := s.db.Exec(
		`INSERT INTO servers(id, token, name, grp, region, flag, note, expire_at, created_at)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, token, f.Name, f.Group, f.Region, f.Flag, f.Note, f.ExpireAt, time.Now().Unix(),
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
	res, err := s.db.Exec(
		`UPDATE servers SET name=?, grp=?, region=?, flag=?, note=?, expire_at=? WHERE id=?`,
		f.Name, f.Group, f.Region, f.Flag, f.Note, f.ExpireAt, id)
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
	rows, err := s.db.Query(`SELECT id, name, type, target, interval, enabled, server_id FROM ping_tasks ORDER BY id`)
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
	res, err := s.db.Exec(
		`INSERT INTO ping_tasks(name, type, target, interval, enabled, server_id) VALUES(?, ?, ?, ?, ?, ?)`,
		t.Name, t.Type, t.Target, t.Interval, t.Enabled, t.ServerID)
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

/* ---------- 站点设置 ---------- */

type settingsView struct {
	SiteName       string `json:"siteName"`
	SiteDesc       string `json:"siteDesc"`
	ReportInterval int    `json:"reportInterval"`
	SampleInterval int    `json:"sampleInterval"`
	HistoryDays    int    `json:"historyDays"`
	PingDays       int    `json:"pingDays"`
}

func (s *App) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, settingsView{
		SiteName:       getSetting(s.db, "site_name", "Moss"),
		SiteDesc:       getSetting(s.db, "site_desc", "轻量服务器监控"),
		ReportInterval: getSettingInt(s.db, "report_interval", 2),
		SampleInterval: getSettingInt(s.db, "sample_interval", 5),
		HistoryDays:    getSettingInt(s.db, "history_days", 30),
		PingDays:       getSettingInt(s.db, "ping_days", 7),
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
		writeErr(w, 400, "请求格式错误")
		return
	}
	if v.SiteName == "" {
		v.SiteName = "Moss"
	}
	setSetting(s.db, "site_name", v.SiteName)
	setSetting(s.db, "site_desc", v.SiteDesc)
	setSetting(s.db, "report_interval", strconv.Itoa(clampInt(v.ReportInterval, 1, 60, 2)))
	setSetting(s.db, "sample_interval", strconv.Itoa(clampInt(v.SampleInterval, 1, 60, 5)))
	setSetting(s.db, "history_days", strconv.Itoa(clampInt(v.HistoryDays, 1, 365, 30)))
	setSetting(s.db, "ping_days", strconv.Itoa(clampInt(v.PingDays, 1, 90, 7)))
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
	setSetting(s.db, "password_hash", string(hash))
	// 修改密码后吊销所有会话
	s.db.Exec(`DELETE FROM sessions`)
	writeJSON(w, 200, map[string]bool{"ok": true})
}
