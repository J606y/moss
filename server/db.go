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
	uuid TEXT NOT NULL DEFAULT '',
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
	expire_notified TEXT NOT NULL DEFAULT '',
	gcp_enabled INTEGER NOT NULL DEFAULT 0,
	gcp_project TEXT NOT NULL DEFAULT '',
	gcp_zone TEXT NOT NULL DEFAULT '',
	gcp_instance TEXT NOT NULL DEFAULT '',
	gcp_cred_id TEXT NOT NULL DEFAULT ''
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
CREATE TABLE IF NOT EXISTS exec_audit (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	job_id TEXT NOT NULL,
	server_id TEXT NOT NULL,
	caller TEXT NOT NULL DEFAULT '',
	cmd TEXT NOT NULL,
	dir TEXT NOT NULL DEFAULT '',
	timeout INTEGER NOT NULL DEFAULT 0,
	started_at INTEGER NOT NULL,
	finished_at INTEGER NOT NULL DEFAULT 0,
	exit_code INTEGER NOT NULL DEFAULT 0,
	error TEXT NOT NULL DEFAULT '',
	stdout TEXT NOT NULL DEFAULT '',
	stderr TEXT NOT NULL DEFAULT '',
	truncated INTEGER NOT NULL DEFAULT 0,
	detach INTEGER NOT NULL DEFAULT 0,
	unit TEXT NOT NULL DEFAULT '',
	canceled INTEGER NOT NULL DEFAULT 0,
	log_bytes INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_exec_audit_job ON exec_audit(job_id);
CREATE INDEX IF NOT EXISTS idx_exec_audit_time ON exec_audit(started_at);
CREATE TABLE IF NOT EXISTS api_keys (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL,
	key_hash TEXT NOT NULL,
	key_prefix TEXT NOT NULL DEFAULT '',
	caps TEXT NOT NULL DEFAULT '',
	servers TEXT NOT NULL DEFAULT '',
	expires_at INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	last_used_at INTEGER NOT NULL DEFAULT 0,
	revoked INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_api_keys_hash ON api_keys(key_hash);
CREATE TABLE IF NOT EXISTS gcp_credentials (
	id TEXT PRIMARY KEY,
	project_id TEXT NOT NULL,
	client_email TEXT NOT NULL,
	sa_json TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_gcp_cred_email ON gcp_credentials(client_email);
CREATE TABLE IF NOT EXISTS oauth_clients (
	client_id TEXT PRIMARY KEY,
	client_name TEXT NOT NULL DEFAULT '',
	redirect_uris TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
`

func pruneExecAudit(db *sql.DB, now time.Time, days, maxRows int) {
	if _, err := db.Exec(
		`DELETE FROM exec_audit WHERE started_at < ?`,
		now.AddDate(0, 0, -days).UnixMilli(),
	); err != nil {
		log.Printf("按保留期清理 exec_audit 失败: %v", err)
	}
	if maxRows <= 0 {
		return
	}
	if _, err := db.Exec(
		`DELETE FROM exec_audit WHERE id <= (SELECT id FROM exec_audit ORDER BY id DESC LIMIT 1 OFFSET ?)`,
		maxRows,
	); err != nil {
		log.Printf("按条数上限清理 exec_audit 失败: %v", err)
	}
}

var defaultSettings = map[string]string{
	keySiteName:         "Moss",
	keySiteDesc:         "智控中心",
	keyReportInterval:   "2",
	keySampleInterval:   "10",
	keyHistoryDays:      "7",
	keyPingDays:         "7",
	keyExecAuditDays:    "90",
	keyExecAuditMaxRows: "5000",
}

const (
	execAuditMinRows = 100
	execAuditMaxRows = 5000
)

func openDB(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	if err := ensureColumn(db, "servers", "uuid", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return nil, err
	}
	if err := ensureColumn(db, "servers", "auto_flag", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return nil, err
	}
	if err := ensureColumn(db, "servers", "ipv6", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return nil, err
	}
	if err := ensureColumn(db, "ping_tasks", "sort", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return nil, err
	}
	if err := ensureColumn(db, "servers", "expire_notified", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return nil, err
	}
	for col, def := range map[string]string{
		"gcp_enabled":  "INTEGER NOT NULL DEFAULT 0",
		"gcp_project":  "TEXT NOT NULL DEFAULT ''",
		"gcp_zone":     "TEXT NOT NULL DEFAULT ''",
		"gcp_instance": "TEXT NOT NULL DEFAULT ''",
		"gcp_cred_id":  "TEXT NOT NULL DEFAULT ''",
	} {
		if err := ensureColumn(db, "servers", col, def); err != nil {
			return nil, err
		}
	}
	for col, def := range map[string]string{
		"detach":    "INTEGER NOT NULL DEFAULT 0",
		"unit":      "TEXT NOT NULL DEFAULT ''",
		"canceled":  "INTEGER NOT NULL DEFAULT 0",
		"log_bytes": "INTEGER NOT NULL DEFAULT 0",
	} {
		if err := ensureColumn(db, "exec_audit", col, def); err != nil {
			return nil, err
		}
	}
	if getSetting(db, keySampleUnitSec, "") == "" {
		var v string
		if err := db.QueryRow(`SELECT value FROM settings WHERE key = ?`, keySampleInterval).Scan(&v); err == nil {
			if n, e := strconv.Atoi(v); e == nil && n > 0 {
				setSetting(db, keySampleInterval, strconv.Itoa(n*60))
			}
		}
		if getSetting(db, keyHistoryDays, "") == "30" {
			setSetting(db, keyHistoryDays, "7")
		}
		setSetting(db, keySampleUnitSec, "1")
	}
	if getSetting(db, keySiteDesc, "") == "轻量服务器监控" {
		setSetting(db, keySiteDesc, "智控中心")
	}
	for k, v := range defaultSettings {
		if _, err := db.Exec(`INSERT OR IGNORE INTO settings(key, value) VALUES(?, ?)`, k, v); err != nil {
			return nil, err
		}
	}
	return db, nil
}

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
			return nil
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

func cleanupLoop(db *sql.DB) {
	for {
		histDays := getSettingInt(db, keyHistoryDays, 7)
		pingDays := getSettingInt(db, keyPingDays, 7)
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
		pruneExecAudit(db, now,
			getSettingInt(db, keyExecAuditDays, 90),
			getSettingInt(db, keyExecAuditMaxRows, execAuditMaxRows))
		gcLoginAttempts(now)
		gcOAuthCodes(now)
		pruneOAuthClients(db, oauthMaxClients)
		time.Sleep(time.Hour)
	}
}
