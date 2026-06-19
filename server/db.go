package main

import (
	"crypto/rand"
	"database/sql"
	"fmt"
	"log"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS servers (
	id TEXT PRIMARY KEY,
	token TEXT UNIQUE NOT NULL,
	name TEXT NOT NULL,
	grp TEXT NOT NULL DEFAULT '默认',
	region TEXT NOT NULL DEFAULT '',
	flag TEXT NOT NULL DEFAULT '',
	note TEXT NOT NULL DEFAULT '',
	expire_at TEXT NOT NULL DEFAULT '',
	sort INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	os TEXT NOT NULL DEFAULT '',
	arch TEXT NOT NULL DEFAULT '',
	virt TEXT NOT NULL DEFAULT '',
	cpu_model TEXT NOT NULL DEFAULT '',
	cpu_cores INTEGER NOT NULL DEFAULT 0,
	mem_total INTEGER NOT NULL DEFAULT 0,
	swap_total INTEGER NOT NULL DEFAULT 0,
	disk_total INTEGER NOT NULL DEFAULT 0,
	agent_version TEXT NOT NULL DEFAULT '',
	ip TEXT NOT NULL DEFAULT '',
	last_seen INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS history (
	server_id TEXT NOT NULL,
	time INTEGER NOT NULL,
	cpu REAL NOT NULL DEFAULT 0,
	mem REAL NOT NULL DEFAULT 0,
	swap REAL NOT NULL DEFAULT 0,
	disk REAL NOT NULL DEFAULT 0,
	load1 REAL NOT NULL DEFAULT 0,
	net_up REAL NOT NULL DEFAULT 0,
	net_down REAL NOT NULL DEFAULT 0,
	tcp INTEGER NOT NULL DEFAULT 0,
	processes INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_history ON history(server_id, time);
CREATE TABLE IF NOT EXISTS ping_results (
	task_id INTEGER NOT NULL,
	server_id TEXT NOT NULL,
	time INTEGER NOT NULL,
	ms INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_ping ON ping_results(server_id, task_id, time);
CREATE TABLE IF NOT EXISTS ping_tasks (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL,
	type TEXT NOT NULL,
	target TEXT NOT NULL,
	interval INTEGER NOT NULL DEFAULT 60,
	enabled INTEGER NOT NULL DEFAULT 1,
	server_id TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS sessions (token TEXT PRIMARY KEY, expires INTEGER NOT NULL);
`

var defaultSettings = map[string]string{
	"site_name":       "Moss",
	"site_desc":       "轻量服务器监控",
	"report_interval": "2",
	"sample_interval": "5",
	"history_days":    "30",
	"ping_days":       "7",
}

func openDB(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// WAL + busy_timeout 下少量并发连接安全；只留 1 个连接会在
	// “rows 未关闭期间再发查询”时直接死锁，放宽到 4 兜底
	db.SetMaxOpenConns(4)
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	for k, v := range defaultSettings {
		if _, err := db.Exec(`INSERT OR IGNORE INTO settings(key, value) VALUES(?, ?)`, k, v); err != nil {
			return nil, err
		}
	}
	return db, nil
}

func getSetting(db *sql.DB, key, fallback string) string {
	var v string
	if err := db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v); err != nil {
		return fallback
	}
	return v
}

func setSetting(db *sql.DB, key, value string) error {
	_, err := db.Exec(`INSERT INTO settings(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

func getSettingInt(db *sql.DB, key string, fallback int) int {
	var n int
	if _, err := fmt.Sscanf(getSetting(db, key, fmt.Sprint(fallback)), "%d", &n); err != nil || n <= 0 {
		return fallback
	}
	return n
}

const tokenChars = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

func randString(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	for i := range b {
		b[i] = tokenChars[int(b[i])%len(tokenChars)]
	}
	return string(b)
}

func newToken() string { return "mk_" + randString(16) }

// cleanupLoop 周期清理过期历史数据与会话。
func cleanupLoop(db *sql.DB) {
	for {
		histDays := getSettingInt(db, "history_days", 30)
		pingDays := getSettingInt(db, "ping_days", 7)
		now := time.Now()
		if _, err := db.Exec(`DELETE FROM history WHERE time < ?`, now.AddDate(0, 0, -histDays).UnixMilli()); err != nil {
			log.Printf("清理 history 失败: %v", err)
		}
		if _, err := db.Exec(`DELETE FROM ping_results WHERE time < ?`, now.AddDate(0, 0, -pingDays).UnixMilli()); err != nil {
			log.Printf("清理 ping_results 失败: %v", err)
		}
		if _, err := db.Exec(`DELETE FROM sessions WHERE expires < ?`, now.Unix()); err != nil {
			log.Printf("清理 sessions 失败: %v", err)
		}
		time.Sleep(time.Hour)
	}
}
