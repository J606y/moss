package main

import (
	"crypto/rand"
	"database/sql"
	"fmt"
	"log"
	"strconv"
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
	ipv6 TEXT NOT NULL DEFAULT '',
	last_seen INTEGER NOT NULL DEFAULT 0,
	auto_flag TEXT NOT NULL DEFAULT '',
	expire_notified TEXT NOT NULL DEFAULT ''
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
	server_id TEXT NOT NULL DEFAULT '',
	sort INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS sessions (token TEXT PRIMARY KEY, expires INTEGER NOT NULL);
`

var defaultSettings = map[string]string{
	"site_name":       "Moss",
	"site_desc":       "轻量服务器监控",
	"report_interval": "2",
	"sample_interval": "10", // 秒：历史落库最细粒度，决定时间轴可缩放的下限
	"history_days":    "7",
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
	// 老库迁移：CREATE TABLE IF NOT EXISTS 不会给已有表加新列，按需补齐。
	if err := ensureColumn(db, "servers", "auto_flag", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return nil, err
	}
	if err := ensureColumn(db, "servers", "ipv6", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return nil, err
	}
	// 老库全为 0，ORDER BY sort, id 仍保持原有按 id 的顺序
	if err := ensureColumn(db, "ping_tasks", "sort", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return nil, err
	}
	// 到期提醒去重标记：记录已就哪个到期日提醒过，防重启/每轮检查重复推送
	if err := ensureColumn(db, "servers", "expire_notified", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return nil, err
	}
	// sample_interval 单位由「分钟」改为「秒」：老库存的是分钟值，首次升级一次性 ×60 迁移，
	// 保持原有落库节奏不变；同时把仍是旧默认(30 天)的历史保留期下调到 7 天，配合更细的采样控制体积。
	if getSetting(db, "sample_unit_sec", "") == "" {
		var v string
		if err := db.QueryRow(`SELECT value FROM settings WHERE key = 'sample_interval'`).Scan(&v); err == nil {
			if n, e := strconv.Atoi(v); e == nil && n > 0 {
				setSetting(db, "sample_interval", strconv.Itoa(n*60))
			}
		}
		if getSetting(db, "history_days", "") == "30" {
			setSetting(db, "history_days", "7")
		}
		setSetting(db, "sample_unit_sec", "1")
	}
	for k, v := range defaultSettings {
		if _, err := db.Exec(`INSERT OR IGNORE INTO settings(key, value) VALUES(?, ?)`, k, v); err != nil {
			return nil, err
		}
	}
	return db, nil
}

// ensureColumn 在列不存在时给表追加列（SQLite 不支持 ADD COLUMN IF NOT EXISTS）。
func ensureColumn(db *sql.DB, table, col, def string) error {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid, notnull, pk int
			name, ctype      string
			dflt             sql.NullString
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == col {
			return nil // 已存在
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + col + ` ` + def)
	return err
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
	// 无偏采样：拒绝落在 [limit, 256) 区间的字节，避免取模偏置使前若干字符概率偏高。
	limit := 256 - (256 % len(tokenChars))
	out := make([]byte, 0, n)
	buf := make([]byte, n)
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			panic(err)
		}
		for _, b := range buf {
			if int(b) >= limit {
				continue
			}
			out = append(out, tokenChars[int(b)%len(tokenChars)])
			if len(out) == n {
				break
			}
		}
	}
	return string(out)
}

func newToken() string { return "mk_" + randString(16) }

// cleanupLoop 周期清理过期历史数据与会话。
func cleanupLoop(db *sql.DB) {
	for {
		histDays := getSettingInt(db, "history_days", 7)
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
		gcLoginAttempts(now) // 回收登录失败限流的内存 map，防无界增长
		time.Sleep(time.Hour)
	}
}
