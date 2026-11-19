// MCP 工具集扩展实现：batch_exec（批量并发执行）、cancel（取消任务）、list_jobs（任务列表）、read_file（分块文件拉取）、write_file（原子文件写入）与 upgrade_agent（Agent 版本查询与受控升级）。
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"moss/internal/protocol"
)

/* ---------- batch_exec ---------- */

type batchExecNodeResult struct {
	ServerID   string `json:"serverId"`
	ServerName string `json:"serverName,omitempty"`
	Group      string `json:"group,omitempty"`
	Online     bool   `json:"online"`
	JobID      string `json:"jobId,omitempty"`
	Status     string `json:"status"`
	Running    bool   `json:"running"`
	OK         bool   `json:"ok"`
	ExitCode   int    `json:"exitCode"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	Output     string `json:"output"`
	Error      string `json:"error,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
	DurationMs int64  `json:"durationMs"`
}

func (s *App) toolBatchExec(r *http.Request, key *apiKey, raw json.RawMessage) mcpCallToolResult {
	if f := requireCap(key, capExec); f != nil {
		return *f
	}
	var args struct {
		Cmd            string   `json:"cmd"`
		ServerIDs      []string `json:"server_ids"`
		Group          string   `json:"group"`
		Dir            string   `json:"dir"`
		Timeout        int      `json:"timeout"`
		IncludeOffline bool     `json:"include_offline"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return toolFailure("参数解析失败: " + err.Error())
	}
	args.Cmd = strings.TrimSpace(args.Cmd)
	if args.Cmd == "" {
		return toolFailure("cmd 不能为空")
	}

	timeout := args.Timeout
	if timeout <= 0 {
		timeout = 60
	}
	if timeout > protocol.ExecMaxTimeout {
		timeout = protocol.ExecMaxTimeout
	}

	type targetNode struct {
		id     string
		name   string
		group  string
		online bool
	}
	var targets []targetNode

	if len(args.ServerIDs) > 0 {
		seen := make(map[string]bool, len(args.ServerIDs))
		for _, rawID := range args.ServerIDs {
			sid, f := s.resolveTarget(key, rawID)
			if f != nil {
				return *f
			}
			if seen[sid] {
				continue
			}
			seen[sid] = true
			var name, grp string
			_ = s.db.QueryRow(`SELECT name, grp FROM servers WHERE id = ?`, sid).Scan(&name, &grp)
			_, _, online := s.hub.Snapshot(sid)
			if s.hub.AgentConn(sid) != nil {
				online = true
			}
			targets = append(targets, targetNode{id: sid, name: name, group: grp, online: online})
		}
	} else {
		rows, err := s.db.Query(`SELECT id, name, grp FROM servers ORDER BY sort, id`)
		if err != nil {
			return toolFailure("查询服务器列表失败: " + err.Error())
		}
		for rows.Next() {
			var t targetNode
			if err := rows.Scan(&t.id, &t.name, &t.group); err != nil {
				rows.Close()
				return toolFailure("读取服务器列表失败: " + err.Error())
			}
			if !key.canAccess(t.id) {
				continue
			}
			if args.Group != "" && !strings.EqualFold(strings.TrimSpace(args.Group), t.group) {
				continue
			}
			_, _, t.online = s.hub.Snapshot(t.id)
			if s.hub.AgentConn(t.id) != nil {
				t.online = true
			}
			if !t.online && !args.IncludeOffline {
				continue
			}
			targets = append(targets, t)
		}
		rows.Close()
	}

	if len(targets) == 0 {
		return jsonResult(map[string]any{
			"status":      "completed",
			"total":       0,
			"completed":   0,
			"succeeded":   0,
			"nonZeroExit": 0,
			"failed":      0,
			"offline":     0,
			"durationMs":  int64(0),
			"results":     []batchExecNodeResult{},
			"note":        "没有匹配到可执行的在线服务器。",
		})
	}

	if why := checkCommand(args.Cmd, args.Dir); why != "" {
		out, _ := s.exec.Submit(r.Context(), s.hub, targets[0].id, s.callerLabel(key), protocol.ExecTask{
			Cmd:     args.Cmd,
			Dir:     args.Dir,
			Timeout: timeout,
		})
		return toolFailure(out.Error + "。这是服务端的安全拦截，改写命令绕过是不允许的；如确有必要请让人工上机操作。")
	}

	batchStart := time.Now()
	results := make([]batchExecNodeResult, len(targets))
	var wg sync.WaitGroup

	for i, tgt := range targets {
		if !tgt.online && s.hub.AgentConn(tgt.id) == nil {
			results[i] = batchExecNodeResult{
				ServerID:   tgt.id,
				ServerName: tgt.name,
				Group:      tgt.group,
				Online:     false,
				Status:     "offline",
				Running:    false,
				OK:         false,
				Error:      errAgentOffline.Error(),
			}
			continue
		}
		wg.Add(1)
		go func(idx int, node targetNode) {
			defer wg.Done()
			out, _ := s.exec.Submit(r.Context(), s.hub, node.id, s.callerLabel(key), protocol.ExecTask{
				Cmd:     args.Cmd,
				Dir:     args.Dir,
				Timeout: timeout,
			})
			st := classifyExecStatus(false, out.Error)
			results[idx] = batchExecNodeResult{
				ServerID:   node.id,
				ServerName: node.name,
				Group:      node.group,
				Online:     st != "offline",
				JobID:      out.JobID,
				Status:     st,
				Running:    false,
				OK:         out.Error == "" && out.ExitCode == 0,
				ExitCode:   out.ExitCode,
				Stdout:     out.Stdout,
				Stderr:     out.Stderr,
				Output:     combineOutput(out.Stdout, out.Stderr),
				Error:      out.Error,
				Truncated:  out.Truncated,
				DurationMs: out.DurationMs,
			}
		}(i, tgt)
	}
	wg.Wait()

	var completed, succeeded, nonZeroExit, failed, offline int
	for _, r := range results {
		switch r.Status {
		case "completed":
			completed++
			if r.ExitCode == 0 {
				succeeded++
			} else {
				nonZeroExit++
			}
		case "offline":
			offline++
		default:
			failed++
		}
	}

	return jsonResult(map[string]any{
		"status":      "completed",
		"total":       len(results),
		"completed":   completed,
		"succeeded":   succeeded,
		"nonZeroExit": nonZeroExit,
		"failed":      failed,
		"offline":     offline,
		"durationMs":  time.Since(batchStart).Milliseconds(),
		"results":     results,
	})
}

/* ---------- cancel ---------- */

func (s *App) toolCancel(key *apiKey, raw json.RawMessage) mcpCallToolResult {
	if f := requireCap(key, capExec); f != nil {
		return *f
	}
	var args struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return toolFailure("参数解析失败: " + err.Error())
	}
	args.JobID = strings.TrimSpace(args.JobID)
	if args.JobID == "" {
		return toolFailure("job_id 不能为空")
	}

	meta, err := s.exec.lookupAuditJob(args.JobID)
	if err != nil {
		if out, serverID, running, found := s.exec.Result(args.JobID); found {
			if _, f := s.resolveTarget(key, serverID); f != nil {
				return *f
			}
			if !running {
				return jsonResult(map[string]any{
					"jobId":    args.JobID,
					"serverId": serverID,
					"running":  false,
					"canceled": false,
					"status":   "already_finished",
					"exitCode": out.ExitCode,
					"note":     "任务已执行结束，无需取消。",
				})
			}
		}
		return toolFailure("找不到任务 " + args.JobID + "：jobId 有误，或记录已被清理。")
	}

	if _, f := s.resolveTarget(key, meta.ServerID); f != nil {
		return *f
	}

	alreadyFinished, cancelErr := s.exec.Cancel(s.hub, meta)
	if cancelErr != nil {
		return toolFailure("取消任务失败: " + cancelErr.Error())
	}
	if alreadyFinished {
		if latest, err := s.exec.lookupAuditJob(args.JobID); err == nil {
			meta = latest
		}
		return jsonResult(map[string]any{
			"jobId":    meta.JobID,
			"serverId": meta.ServerID,
			"running":  false,
			"canceled": meta.Canceled,
			"status":   "already_finished",
			"exitCode": meta.ExitCode,
			"note":     "任务此前已执行结束，无需取消。",
		})
	}
	return jsonResult(map[string]any{
		"jobId":    meta.JobID,
		"serverId": meta.ServerID,
		"detached": meta.Detach,
		"unit":     meta.Unit,
		"running":  false,
		"canceled": true,
		"status":   "canceled",
	})
}

/* ---------- list_jobs ---------- */

type mcpJobItem struct {
	JobID      string `json:"jobId"`
	ServerID   string `json:"serverId"`
	ServerName string `json:"serverName,omitempty"`
	Cmd        string `json:"cmd"`
	Dir        string `json:"dir,omitempty"`
	Detach     bool   `json:"detach,omitempty"`
	Unit       string `json:"unit,omitempty"`
	Timeout    int    `json:"timeout"`
	Running    bool   `json:"running"`
	Status     string `json:"status"`
	OK         bool   `json:"ok"`
	Canceled   bool   `json:"canceled,omitempty"`
	ExitCode   int    `json:"exitCode"`
	Error      string `json:"error,omitempty"`
	LogBytes   int64  `json:"logBytes,omitempty"`
	StartedAt  int64  `json:"startedAt"`
	FinishedAt int64  `json:"finishedAt,omitempty"`
	DurationMs int64  `json:"durationMs"`
}

func (s *App) toolListJobs(key *apiKey, raw json.RawMessage) mcpCallToolResult {
	if f := requireCap(key, capExec); f != nil {
		return *f
	}
	var args struct {
		ServerID    string `json:"server_id"`
		RunningOnly bool   `json:"running_only"`
		Limit       int    `json:"limit"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return toolFailure("参数解析失败: " + err.Error())
		}
	}
	args.ServerID = strings.TrimSpace(args.ServerID)
	if args.ServerID != "" {
		sid, f := s.resolveTarget(key, args.ServerID)
		if f != nil {
			return *f
		}
		args.ServerID = sid
	}
	limit := args.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}

	query := `
		SELECT a.job_id, a.server_id, COALESCE(s.name, ''), a.cmd, a.dir, a.timeout,
		       a.detach, a.unit, a.started_at, a.finished_at, a.exit_code, a.error,
		       a.canceled, a.log_bytes
		  FROM exec_audit a LEFT JOIN servers s ON s.id = a.server_id
		 WHERE a.cmd NOT LIKE '[write_file] %' AND a.cmd NOT LIKE '[upgrade_agent] %'`
	qArgs := []any{}
	if args.ServerID != "" {
		query += ` AND a.server_id = ?`
		qArgs = append(qArgs, args.ServerID)
	}
	query += ` ORDER BY a.started_at DESC, a.id DESC LIMIT 200`

	rows, err := s.db.Query(query, qArgs...)
	if err != nil {
		return toolFailure("查询任务列表失败: " + err.Error())
	}
	var candidates []mcpJobItem
	for rows.Next() {
		var (
			it               mcpJobItem
			detach, canceled int
		)
		if err := rows.Scan(
			&it.JobID, &it.ServerID, &it.ServerName, &it.Cmd, &it.Dir, &it.Timeout,
			&detach, &it.Unit, &it.StartedAt, &it.FinishedAt, &it.ExitCode, &it.Error,
			&canceled, &it.LogBytes,
		); err != nil {
			rows.Close()
			return toolFailure("读取任务列表失败: " + err.Error())
		}
		if !key.canAccess(it.ServerID) {
			continue
		}
		it.Detach = detach == 1
		it.Canceled = canceled == 1
		candidates = append(candidates, it)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return toolFailure("读取任务列表失败: " + err.Error())
	}
	rows.Close()

	nowMs := time.Now().UnixMilli()
	list := make([]mcpJobItem, 0, limit)
	for _, it := range candidates {
		if it.FinishedAt == 0 {
			if it.Detach {
				reqID := "l_" + randString(16)
				if st, err := s.exec.requestJobStatus(s.hub, it.ServerID, protocol.ServerMsg{
					Type: "job_query",
					JobQuery: &protocol.JobQuery{
						ReqID: reqID,
						JobID: it.JobID,
						Limit: -1,
					},
				}, reqID, 2*time.Second); err == nil && st.Found {
					s.exec.syncDetachedAudit(it.JobID, st)
					it.LogBytes = st.TotalBytes
					if st.Unit != "" {
						it.Unit = st.Unit
					}
					if st.Running {
						it.Running = true
						it.Status = "running"
						it.DurationMs = nowMs - it.StartedAt
					} else {
						it.Running = false
						it.FinishedAt = nowMs
						it.ExitCode = st.ExitCode
						it.Error = st.Error
						it.Canceled = st.Canceled
						it.OK = it.Error == "" && it.ExitCode == 0
						it.DurationMs = it.FinishedAt - it.StartedAt
						it.Status = classifyExecStatus(it.Canceled, it.Error)
					}
				} else {
					it.Running = true
					it.Status = "running"
					it.DurationMs = nowMs - it.StartedAt
				}
			} else if s.exec.isRunningInMemory(it.JobID) {
				it.Running = true
				it.Status = "running"
				it.DurationMs = nowMs - it.StartedAt
			} else {
				it.Running = false
				it.Status = "lost"
				if it.Error == "" {
					it.Error = "服务端曾重启，非 detach 任务连接已断开"
				}
			}
		} else {
			it.Running = false
			it.OK = it.Error == "" && it.ExitCode == 0
			it.DurationMs = it.FinishedAt - it.StartedAt
			it.Status = classifyExecStatus(it.Canceled, it.Error)
		}

		if args.RunningOnly && !it.Running {
			continue
		}
		list = append(list, it)
		if len(list) >= limit {
			break
		}
	}

	return jsonResult(map[string]any{
		"status": "completed",
		"jobs":   list,
		"count":  len(list),
	})
}

/* ---------- read_file ---------- */

func (s *App) toolReadFile(key *apiKey, raw json.RawMessage) mcpCallToolResult {
	if f := requireCap(key, capRead); f != nil {
		return *f
	}
	var args struct {
		ServerID string `json:"server_id"`
		Path     string `json:"path"`
		Offset   int64  `json:"offset"`
		Length   int    `json:"length"`
		Encoding string `json:"encoding"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return toolFailure("参数解析失败: " + err.Error())
	}
	args.Path = strings.TrimSpace(args.Path)
	if args.Path == "" {
		return toolFailure("path 不能为空")
	}
	if !strings.HasPrefix(args.Path, "/") && !isWindowsAbs(args.Path) {
		return toolFailure("path 必须是绝对路径")
	}
	enc := strings.ToLower(strings.TrimSpace(args.Encoding))
	if enc == "" {
		enc = "auto"
	}
	if enc != "auto" && enc != "utf8" && enc != "base64" {
		return toolFailure("encoding 仅支持 \"auto\"、\"utf8\" 或 \"base64\"")
	}
	serverID, f := s.resolveTarget(key, args.ServerID)
	if f != nil {
		return *f
	}

	res, err := s.exec.SubmitRead(s.hub, serverID, args.Path, args.Offset, args.Length)
	if err != nil {
		return toolFailure("读取文件失败: " + err.Error())
	}

	actualEnc := enc
	if actualEnc == "auto" {
		if utf8.Valid(res.Data) && !bytes.ContainsRune(res.Data, '\x00') {
			actualEnc = "utf8"
		} else {
			actualEnc = "base64"
		}
	}
	var content string
	if actualEnc == "base64" {
		content = base64.StdEncoding.EncodeToString(res.Data)
	} else {
		content = string(res.Data)
	}
	sum := sha256.Sum256(res.Data)

	return jsonResult(map[string]any{
		"status":     "completed",
		"serverId":   serverID,
		"path":       args.Path,
		"encoding":   actualEnc,
		"content":    content,
		"bytes":      len(res.Data),
		"offset":     res.Offset,
		"nextOffset": res.NextOffset,
		"totalBytes": res.TotalBytes,
		"hasMore":    !res.EOF,
		"eof":        res.EOF,
		"sha256":     hex.EncodeToString(sum[:]),
	})
}

/* ---------- write_file ---------- */

func (s *App) toolWriteFile(r *http.Request, key *apiKey, raw json.RawMessage) mcpCallToolResult {
	if f := requireCap(key, capWrite); f != nil {
		return *f
	}
	var args struct {
		ServerID string `json:"server_id"`
		Path     string `json:"path"`
		Content  string `json:"content"`
		Mode     string `json:"mode"`
		Append   bool   `json:"append"`
		Mkdir    bool   `json:"mkdir"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return toolFailure("参数解析失败: " + err.Error())
	}
	if strings.TrimSpace(args.Path) == "" {
		return toolFailure("path 不能为空")
	}
	if !strings.HasPrefix(args.Path, "/") && !isWindowsAbs(args.Path) {
		return toolFailure("path 必须是绝对路径")
	}
	if len(args.Content) > protocol.WriteSizeCap {
		return toolFailure(fmt.Sprintf("内容 %d 字节超过上限 %d，请改用 exec 在目标机上拉取大文件",
			len(args.Content), protocol.WriteSizeCap))
	}
	mode, err := parseFileMode(args.Mode)
	if err != nil {
		return toolFailure(err.Error())
	}
	serverID, f := s.resolveTarget(key, args.ServerID)
	if f != nil {
		return *f
	}

	out, err := s.exec.SubmitWrite(r.Context(), s.hub, serverID, s.callerLabel(key), protocol.WriteTask{
		Path:   args.Path,
		Data:   []byte(args.Content),
		Mode:   mode,
		Append: args.Append,
		Mkdir:  args.Mkdir,
	})
	if err != nil || out.Error != "" {
		msg := out.Error
		if msg == "" && err != nil {
			msg = err.Error()
		}
		if errors.Is(err, errExecBlocked) {
			msg += "。这是服务端的安全拦截，请让人工上机操作。"
		}
		return toolFailure("写入失败: " + msg)
	}
	return jsonResult(map[string]any{
		"status":   "completed",
		"serverId": serverID,
		"path":     args.Path,
		"bytes":    len(args.Content),
		"mode":     fmt.Sprintf("%o", mode),
		"appended": args.Append,
	})
}

/* ---------- upgrade_agent ---------- */

func (s *App) toolUpgradeAgent(key *apiKey, raw json.RawMessage) mcpCallToolResult {
	var args struct {
		ServerID string `json:"server_id"`
		Action   string `json:"action"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return toolFailure("参数解析失败: " + err.Error())
	}
	action := strings.ToLower(strings.TrimSpace(args.Action))
	if action == "" {
		action = "check"
	}
	if action != "check" && action != "status" && action != "upgrade" {
		return toolFailure("action 仅支持 \"check\"、\"status\" 或 \"upgrade\"")
	}
	if action == "upgrade" {
		if f := requireCap(key, capExec); f != nil {
			return *f
		}
	} else {
		if !key.has(capRead) && !key.has(capExec) {
			if f := requireCap(key, capRead); f != nil {
				return *f
			}
		}
	}

	serverID, f := s.resolveTarget(key, args.ServerID)
	if f != nil {
		return *f
	}

	var name, uuid, agentOS, agentVer string
	if err := s.db.QueryRow(
		`SELECT name, uuid, os, agent_version FROM servers WHERE id = ?`, serverID,
	).Scan(&name, &uuid, &agentOS, &agentVer); err != nil {
		return toolFailure("读取服务器信息失败: " + err.Error())
	}

	_, _, online := s.hub.Snapshot(serverID)
	if s.hub.AgentConn(serverID) != nil {
		online = true
	}
	targetVer := upgradeTarget()
	upgradable, hint := upgradeAvailability(agentVer, agentOS, online)
	stage, upgradeErr := s.upgrade.Status(serverID)

	if action == "check" || action == "status" {
		res := map[string]any{
			"status":          "completed",
			"serverId":        serverID,
			"uuid":            uuid,
			"serverName":      name,
			"os":              agentOS,
			"online":          online,
			"agentVersion":    agentVer,
			"targetVersion":   targetVer,
			"supportsUpgrade": agentSupportsUpgrade(agentVer),
			"upgradable":      upgradable,
			"upToDate":        targetVer != "" && sameVersion(agentVer, targetVer),
		}
		if hint != nil {
			res["hint"] = hint.Error()
			res["hintCode"] = hint.Code
		}
		if stage != "" {
			res["stage"] = stage
		}
		if upgradeErr != "" {
			res["upgradeError"] = upgradeErr
		}
		return jsonResult(res)
	}

	// action == "upgrade"：受控自升级，先落审计记录再由 upgradeManager 下发官方升级指令。
	auditJob := &execJob{
		id:       newExecJobID(),
		serverID: serverID,
		started:  time.Now(),
		done:     make(chan struct{}),
	}
	_ = s.exec.auditStart(auditJob, serverID, s.callerLabel(key), protocol.ExecTask{
		Cmd: fmt.Sprintf("[upgrade_agent] %s -> %s", displayVersion(agentVer), displayVersion(targetVer)),
	})

	if err := s.upgrade.Start(s.hub, serverID, agentVer, agentOS); err != nil {
		s.exec.auditFinish(auditJob.id, ExecOutcome{JobID: auditJob.id, Error: err.Error()})
		return toolFailure("下发 Agent 升级失败: " + err.Error())
	}
	s.exec.auditFinish(auditJob.id, ExecOutcome{
		JobID:  auditJob.id,
		Stdout: fmt.Sprintf("已向 %s 下发受控升级任务 (%s -> %s)", serverID, displayVersion(agentVer), displayVersion(targetVer)),
	})

	stage, _ = s.upgrade.Status(serverID)
	return jsonResult(map[string]any{
		"status":        "running",
		"serverId":      serverID,
		"uuid":          uuid,
		"serverName":    name,
		"fromVersion":   agentVer,
		"targetVersion": targetVer,
		"stage":         stage,
		"message":       "已下发受控升级指令；agent 校验 SHA256 替换二进制后将自动重启，若 60 秒内未能连回将自动回滚。",
	})
}
