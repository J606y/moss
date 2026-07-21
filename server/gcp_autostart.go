package main

// GCP Spot 自动开机的编排与状态机（与告警引擎分居 notify.go）。
// 底层 Compute API 客户端见 gcp.go；将来接第二家云厂商时在此扩展调度逻辑。

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

var errGCPBusy = errors.New("自动开机执行中，请稍候")

// getGCPClient 懒建 GCP 客户端，凭证内容不变时复用（token 缓存随之保留）。
func (n *Notifier) getGCPClient() (*gcpClient, error) {
	raw := strings.TrimSpace(decryptSecret(getSetting(n.db, keyGCPSAJSON, "")))
	if raw == "" {
		return nil, errors.New("未配置 Service Account 凭证")
	}
	n.mu.Lock()
	if n.gcpCli != nil && n.gcpSARaw == raw {
		cli := n.gcpCli
		n.mu.Unlock()
		return cli, nil
	}
	n.mu.Unlock()
	cli, err := newGCPClient(raw)
	if err != nil {
		return nil, err
	}
	n.mu.Lock()
	n.gcpCli = cli
	n.gcpSARaw = raw
	n.mu.Unlock()
	return cli, nil
}

// gcpResolveStatus 获取客户端、补全 project（留空则用 SA 的 project_id）、查询实例状态。
// 自动开机与手动开机路径共用此前半段；返回 cli==nil 表示是建客户端阶段失败，
// 便于调用方区分「凭证/建连失败」与「状态查询失败」施加不同告警。
func (n *Notifier) gcpResolveStatus(ctx context.Context, project, zone, instance string) (cli *gcpClient, proj, status string, err error) {
	cli, err = n.getGCPClient()
	if err != nil {
		return nil, "", "", err
	}
	proj = project
	if proj == "" {
		proj = cli.sa.ProjectID
	}
	status, err = cli.InstanceStatus(ctx, proj, zone, instance)
	return cli, proj, status, err
}

// checkGCPStart 每 tick 从 DB 查启用节点自行记账。不挂在离线告警块内：
// 那里受 OfflineOn 开关控制，且依赖 WS 断连事件，面板重启后会漏掉已死节点。
func (n *Notifier) checkGCPStart() {
	n.mu.Lock()
	cfg := n.gcpCfg
	tgCfg := n.cfg
	n.mu.Unlock()
	if !cfg.AutoOn {
		return
	}
	rows, err := n.db.Query(
		`SELECT id, name, gcp_project, gcp_zone, gcp_instance FROM servers WHERE gcp_enabled = 1`)
	if err != nil {
		log.Printf("checkGCPStart query: %v", err)
		return
	}
	type target struct{ id, name, project, zone, instance string }
	var targets []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.id, &t.name, &t.project, &t.zone, &t.instance); err == nil {
			targets = append(targets, t)
		}
	}
	rows.Close()

	now := time.Now()
	for _, t := range targets {
		if n.isOnline(t.id) {
			n.mu.Lock()
			delete(n.gcp, t.id) // 与 OnOnline 双保险
			n.mu.Unlock()
			continue
		}
		if t.zone == "" || t.instance == "" {
			continue
		}
		n.mu.Lock()
		st, ok := n.gcp[t.id]
		if !ok {
			n.gcp[t.id] = &gcpState{offlineAt: now}
			n.mu.Unlock()
			continue // 刚观察到离线，从此刻起算确认延迟
		}
		due, giveUp := gcpDue(st, cfg, now)
		if giveUp {
			fire := !st.gaveUp
			st.gaveUp = true
			n.mu.Unlock()
			if fire {
				n.send(tgCfg, fmt.Sprintf("🛑 GCP 自动开机已停止\n%s 已尝试 %d 次仍未上线，等待人工处理（节点上线后自动复位）",
					t.name, cfg.MaxTries))
			}
			continue
		}
		if !due {
			n.mu.Unlock()
			continue
		}
		st.inFlight = true
		st.tries++
		st.lastTry = now
		tries := st.tries
		n.mu.Unlock()
		go n.gcpStartAttempt(t.id, t.name, t.project, t.zone, t.instance, tries, cfg, tgCfg)
	}
}

// setGCPErr 记录最近一次错误供前端 tooltip 展示（节点可能已被删/已上线，状态不存在则忽略）。
func (n *Notifier) setGCPErr(id, msg string) {
	n.mu.Lock()
	if st, ok := n.gcp[id]; ok {
		st.lastErr = msg
	}
	n.mu.Unlock()
}

// gcpStartAttempt 单次自动开机尝试（goroutine，不阻塞 Run 循环）。
// 先查实例状态：仅 TERMINATED/STOPPED 才开机，RUNNING 说明是 agent/网络问题，不动。
func (n *Notifier) gcpStartAttempt(id, name, project, zone, instance string, tries int, cfg gcpConfig, tgCfg notifyConfig) {
	defer func() {
		n.mu.Lock()
		if st, ok := n.gcp[id]; ok {
			st.inFlight = false
		}
		n.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cli, project, status, err := n.gcpResolveStatus(ctx, project, zone, instance)
	if err != nil {
		if cli == nil { // 建客户端/凭证失败：仅记录，不发 TG（与查询失败区分）
			n.setGCPErr(id, err.Error())
			log.Printf("GCP 自动开机(%s): %v", name, err)
			return
		}
		n.setGCPErr(id, "查询实例状态失败: "+err.Error())
		log.Printf("GCP 自动开机(%s): 查询状态失败: %v", name, err)
		n.send(tgCfg, fmt.Sprintf("⚠️ GCP 自动开机失败\n%s 第 %d/%d 次：查询实例状态失败：%v", name, tries, cfg.MaxTries, err))
		return
	}
	switch status {
	case "TERMINATED", "STOPPED":
		if err := cli.StartInstance(ctx, project, zone, instance); err != nil {
			n.setGCPErr(id, "instances.start 失败: "+err.Error())
			log.Printf("GCP 自动开机(%s): start 失败: %v", name, err)
			n.send(tgCfg, fmt.Sprintf("⚠️ GCP 自动开机失败\n%s 第 %d/%d 次：%v", name, tries, cfg.MaxTries, err))
			return
		}
		n.setGCPErr(id, "")
		log.Printf("GCP 自动开机(%s): 已调用 instances.start（第 %d/%d 次）", name, tries, cfg.MaxTries)
		n.send(tgCfg, fmt.Sprintf("🔄 GCP 自动开机\n%s 已调用 instances.start（第 %d/%d 次），等待节点上线", name, tries, cfg.MaxTries))
	case "RUNNING":
		n.setGCPErr(id, "实例运行中但节点离线，疑似 agent/网络故障")
		n.mu.Lock()
		st, ok := n.gcp[id]
		fire := ok && !st.warnedRun
		if ok {
			st.warnedRun = true
		}
		n.mu.Unlock()
		if fire {
			n.send(tgCfg, fmt.Sprintf("⚠️ GCP 守护提醒\n%s 实例状态为 RUNNING 但节点离线，可能是 agent 或网络故障，不执行开机", name))
		}
	case "SUSPENDED":
		n.setGCPErr(id, "实例已挂起（SUSPENDED），暂不支持自动恢复")
		log.Printf("GCP 自动开机(%s): 实例 SUSPENDED，跳过", name)
	default:
		// PROVISIONING/STAGING/STOPPING/REPAIRING 等过渡态，冷却后下一轮再看
		log.Printf("GCP 自动开机(%s): 实例状态 %s，跳过本次", name, status)
	}
}

// ManualStartGCP 手动立即开机：忽略冷却、不消耗自动尝试次数，
// 但记录 lastTry 让自动循环退让一个冷却期，避免背靠背双重 start。
func (n *Notifier) ManualStartGCP(id, project, zone, instance string) (status string, started bool, err error) {
	n.mu.Lock()
	st, ok := n.gcp[id]
	if ok && st.inFlight {
		n.mu.Unlock()
		return "", false, errGCPBusy
	}
	if !ok {
		st = &gcpState{offlineAt: time.Now()}
		n.gcp[id] = st
	}
	st.inFlight = true
	st.lastTry = time.Now()
	n.mu.Unlock()
	defer func() {
		n.mu.Lock()
		if st, ok := n.gcp[id]; ok {
			st.inFlight = false
			if err != nil {
				st.lastErr = err.Error()
			}
		}
		n.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cli, project, status, err := n.gcpResolveStatus(ctx, project, zone, instance)
	if err != nil {
		if cli == nil {
			return "", false, err
		}
		return "", false, fmt.Errorf("查询实例状态失败: %w", err)
	}
	if status == "TERMINATED" || status == "STOPPED" {
		if err = cli.StartInstance(ctx, project, zone, instance); err != nil {
			return status, false, err
		}
		log.Printf("GCP 手动开机(%s): 已调用 instances.start", instance)
		return status, true, nil
	}
	return status, false, nil
}

// GCPStatus 导出节点自动开机运行态（内存态，面板重启归零，与冷却一起重置属合理行为）。
func (n *Notifier) GCPStatus(id string) (tries int, lastTry int64, lastErr string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	st, ok := n.gcp[id]
	if !ok {
		return 0, 0, ""
	}
	if !st.lastTry.IsZero() {
		lastTry = st.lastTry.Unix()
	}
	return st.tries, lastTry, st.lastErr
}
