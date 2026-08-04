package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

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

// execAuditPage 审计列表的分页响应。
//
// 带上 Total 是分页的前提：不知道总数就画不出页码，只能退化成「一直往下加载」——
// 而那正是 5000 条堆在一页里、滑到手酸还卡顿的来源。
type execAuditPage struct {
	Items []execAuditRow `json:"items"`
	Total int            `json:"total"`
}

// handleExecAudit 返回执行审计列表，按时间倒序，带总数供前端分页。
func (s *App) handleExecAudit(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	// offset 支持翻页：审计会持续增长，只给最近一页等于旧记录不可达。
	offset := 0
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			offset = n
		}
	}

	// filterArgs 只含筛选条件，供 COUNT 复用；分页参数单独追加，
	// 否则总数会被 LIMIT 影响，页码数永远算错。
	filterArgs := []any{}
	conds := []string{}
	if sid := r.URL.Query().Get("server"); sid != "" {
		conds = append(conds, "a.server_id = ?")
		filterArgs = append(filterArgs, sid)
	}
	// 「仅看拦截」：被拦下的尝试是审计里最该被单独捞出来看的记录，
	// 混在日常执行流水里很快就被刷到翻不到的地方。
	if r.URL.Query().Get("blocked") == "1" {
		conds = append(conds, "a.error LIKE ?")
		filterArgs = append(filterArgs, execBlockedPrefix+"%")
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}

	// 总数与列表必须用同一套筛选条件，否则筛选后页码还是按全量算的。
	var total int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM exec_audit a`+where, filterArgs...,
	).Scan(&total); err != nil {
		log.Printf("handleExecAudit count: %v", err)
		writeErr(w, 500, "内部错误")
		return
	}

	rows, err := s.db.Query(`
		SELECT a.job_id, a.server_id, COALESCE(s.name, ''), a.caller, a.cmd, a.dir,
		       a.started_at, a.finished_at, a.exit_code, a.error, a.truncated
		FROM exec_audit a LEFT JOIN servers s ON s.id = a.server_id`+where+`
		ORDER BY a.started_at DESC LIMIT ? OFFSET ?`, append(filterArgs, limit, offset)...)
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
	writeJSON(w, 200, execAuditPage{Items: list, Total: total})
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
