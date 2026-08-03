package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"

	"moss/internal/protocol"
)

type execRequest struct {
	Cmd     string `json:"cmd"`
	Dir     string `json:"dir"`
	Timeout int    `json:"timeout"`
}

// handleExec 在指定服务器上执行命令并同步返回结果。
//
// 阶段 1 的验证接口：走管理员会话鉴权，尚不涉及 API Key 体系。
// 请求会阻塞到执行结束，因此不适合长任务——这一点在接入 MCP 时需改为异步任务模式。
func (s *App) handleExec(w http.ResponseWriter, r *http.Request) {
	serverID := r.PathValue("id")
	var req execRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Cmd == "" {
		writeErr(w, 400, "参数不完整")
		return
	}

	var exists int
	if err := s.db.QueryRow(`SELECT COUNT(1) FROM servers WHERE id = ?`, serverID).Scan(&exists); err != nil || exists == 0 {
		writeErr(w, 404, "服务器不存在")
		return
	}

	out, err := s.exec.Submit(r.Context(), s.hub, serverID, "admin", protocol.ExecTask{
		Cmd:     req.Cmd,
		Dir:     req.Dir,
		Timeout: req.Timeout,
	})
	switch {
	case errors.Is(err, errExecBlocked):
		writeJSON(w, 403, out)
	case errors.Is(err, errAgentOffline):
		writeJSON(w, 503, out)
	case errors.Is(err, errExecTimeout):
		writeJSON(w, 504, out)
	case err != nil:
		writeJSON(w, 500, out)
	default:
		writeJSON(w, 200, out)
	}
}

// execAuditRow 审计列表项，不含输出正文以控制响应体积。
type execAuditRow struct {
	JobID      string `json:"jobId"`
	ServerID   string `json:"serverId"`
	ServerName string `json:"serverName"`
	Caller     string `json:"caller"`
	Cmd        string `json:"cmd"`
	Dir        string `json:"dir"`
	StartedAt  int64  `json:"startedAt"`
	FinishedAt int64  `json:"finishedAt"`
	ExitCode   int    `json:"exitCode"`
	Error      string `json:"error,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
}

// handleExecAudit 返回执行审计列表，按时间倒序。
func (s *App) handleExecAudit(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	args := []any{}
	where := ""
	if sid := r.URL.Query().Get("server"); sid != "" {
		where = ` WHERE a.server_id = ?`
		args = append(args, sid)
	}
	args = append(args, limit)

	rows, err := s.db.Query(`
		SELECT a.job_id, a.server_id, COALESCE(s.name, ''), a.caller, a.cmd, a.dir,
		       a.started_at, a.finished_at, a.exit_code, a.error, a.truncated
		FROM exec_audit a LEFT JOIN servers s ON s.id = a.server_id`+where+`
		ORDER BY a.started_at DESC LIMIT ?`, args...)
	if err != nil {
		log.Printf("handleExecAudit query: %v", err)
		writeErr(w, 500, "内部错误")
		return
	}
	defer rows.Close()

	list := make([]execAuditRow, 0, limit)
	for rows.Next() {
		var it execAuditRow
		var truncated int
		if err := rows.Scan(&it.JobID, &it.ServerID, &it.ServerName, &it.Caller, &it.Cmd, &it.Dir,
			&it.StartedAt, &it.FinishedAt, &it.ExitCode, &it.Error, &truncated); err != nil {
			log.Printf("handleExecAudit scan: %v", err)
			writeErr(w, 500, "内部错误")
			return
		}
		it.Truncated = truncated == 1
		list = append(list, it)
	}
	if err := rows.Err(); err != nil {
		log.Printf("handleExecAudit rows: %v", err)
		writeErr(w, 500, "内部错误")
		return
	}
	writeJSON(w, 200, list)
}

// handleExecAuditDetail 返回单条审计的完整记录，含输出正文。
func (s *App) handleExecAuditDetail(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("jobId")
	var (
		it        execAuditRow
		stdout    string
		stderr    string
		timeout   int
		truncated int
	)
	err := s.db.QueryRow(`
		SELECT a.job_id, a.server_id, COALESCE(s.name, ''), a.caller, a.cmd, a.dir, a.timeout,
		       a.started_at, a.finished_at, a.exit_code, a.error, a.stdout, a.stderr, a.truncated
		FROM exec_audit a LEFT JOIN servers s ON s.id = a.server_id
		WHERE a.job_id = ?`, jobID).
		Scan(&it.JobID, &it.ServerID, &it.ServerName, &it.Caller, &it.Cmd, &it.Dir, &timeout,
			&it.StartedAt, &it.FinishedAt, &it.ExitCode, &it.Error, &stdout, &stderr, &truncated)
	if err != nil {
		writeErr(w, 404, "记录不存在")
		return
	}
	it.Truncated = truncated == 1
	writeJSON(w, 200, map[string]any{
		"record":  it,
		"timeout": timeout,
		"stdout":  stdout,
		"stderr":  stderr,
	})
}
