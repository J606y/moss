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

// gcpTarget 一台被守护节点的定位信息：用哪份凭证、去哪个项目/可用区找哪台实例。
//
// 用结构体而不是继续加位置参数：加上凭证 id 后这条链路要传六个字段，
// 而其中四个都是字符串，调用点写错顺序编译器一声不吭，出错时却是拿错凭证开错机器。
type gcpTarget struct {
	id       string // 节点 id
	name     string // 节点名，只用于日志与告警文案
	credID   string // 绑定的凭证 id，空表示未绑定（仅存一份凭证时可回退）
	project  string // 留空则用凭证自身的 project_id
	zone     string
	instance string
}

// getGCPClient 懒建 GCP 客户端，按凭证 id 分槽缓存，凭证内容不变时复用（token 缓存随之保留）。
func (n *Notifier) getGCPClient(credID string) (*gcpClient, error) {
	id, err := resolveGCPCredID(n.db, credID)
	if err != nil {
		return nil, err
	}
	cred, err := loadGCPCredential(n.db, id)
	if err != nil {
		return nil, err
	}
	// 用带错误的版本，别让「凭证解不开」伪装成「没配凭证」。
	//
	// 两者的处置完全相反：没配是「去后台填一下」，解不开是「主密钥变了或
	// secret.key 丢了，去把它找回来」。而自动开机是无人值守链路，
	// 报错文案就是运维唯一能拿到的线索——说错了就是把人引向错误的方向。
	// 多凭证下还必须点名是哪一份：两个账号时「凭证无法解密」不说是哪个等于没说。
	stored, err := decryptSecretValue(cred.SaJSON)
	if err != nil {
		return nil, fmt.Errorf("凭证 %s 无法解密（主密钥可能已变更或 secret.key 丢失），请重新添加: %w", cred.ClientEmail, err)
	}
	raw := strings.TrimSpace(stored)
	if raw == "" {
		return nil, fmt.Errorf("凭证 %s 内容为空，请重新添加", cred.ClientEmail)
	}
	n.mu.Lock()
	if slot, ok := n.gcpClients[id]; ok && slot.raw == raw {
		cli := slot.cli
		n.mu.Unlock()
		return cli, nil
	}
	n.mu.Unlock()
	// 建客户端要解析 RSA 私钥，放在锁外：n.mu 同时护着整个告警引擎和
	// 每 15s 的检查循环，不该为一次密钥解析把它们全堵住。
	// 两个 goroutine 同时未命中会各建一个，末位写入者胜出，代价只是多换一次 token。
	cli, err := newGCPClient(raw)
	if err != nil {
		return nil, fmt.Errorf("凭证 %s 无效: %w", cred.ClientEmail, err)
	}
	n.mu.Lock()
	n.gcpClients[id] = &gcpCachedClient{raw: raw, cli: cli}
	n.mu.Unlock()
	return cli, nil
}

// gcpResolveStatus 获取客户端、补全 project（留空则用所绑凭证的 project_id）、查询实例状态。
// 自动开机与手动开机路径共用此前半段；返回 cli==nil 表示是建客户端阶段失败，
// 便于调用方区分「凭证/建连失败」与「状态查询失败」施加不同告警。
func (n *Notifier) gcpResolveStatus(ctx context.Context, t gcpTarget) (cli *gcpClient, proj, status string, err error) {
	cli, err = n.getGCPClient(t.credID)
	if err != nil {
		return nil, "", "", err
	}
	proj = t.project
	if proj == "" {
		proj = cli.sa.ProjectID
	}
	status, err = cli.InstanceStatus(ctx, proj, t.zone, t.instance)
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
		`SELECT id, name, gcp_cred_id, gcp_project, gcp_zone, gcp_instance FROM servers WHERE gcp_enabled = 1`)
	if err != nil {
		log.Printf("checkGCPStart query: %v", err)
		return
	}
	var targets []gcpTarget
	for rows.Next() {
		var t gcpTarget
		if err := rows.Scan(&t.id, &t.name, &t.credID, &t.project, &t.zone, &t.instance); err == nil {
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
				n.fire(tgCfg, alertEvent{
					Type:       evtGCPGaveUp,
					ServerID:   t.id,
					ServerName: t.name,
					Text: fmt.Sprintf("🛑 GCP 自动开机已停止\n%s 已尝试 %d 次仍未上线，等待人工处理（节点上线后自动复位）",
						t.name, cfg.MaxTries),
				})
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
		go n.gcpStartAttempt(t, tries, cfg, tgCfg)
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
func (n *Notifier) gcpStartAttempt(t gcpTarget, tries int, cfg gcpConfig, tgCfg notifyConfig) {
	defer func() {
		n.mu.Lock()
		if st, ok := n.gcp[t.id]; ok {
			st.inFlight = false
		}
		n.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cli, project, status, err := n.gcpResolveStatus(ctx, t)
	if err != nil {
		if cli == nil { // 建客户端/凭证失败：仅记录，不发 TG（与查询失败区分）
			n.setGCPErr(t.id, err.Error())
			log.Printf("GCP 自动开机(%s): %v", t.name, err)
			return
		}
		n.setGCPErr(t.id, "查询实例状态失败: "+err.Error())
		log.Printf("GCP 自动开机(%s): 查询状态失败: %v", t.name, err)
		n.fire(tgCfg, alertEvent{
			Type:       evtGCPFailed,
			ServerID:   t.id,
			ServerName: t.name,
			Text:       fmt.Sprintf("⚠️ GCP 自动开机失败\n%s 第 %d/%d 次：查询实例状态失败：%v", t.name, tries, cfg.MaxTries, err),
		})
		return
	}
	switch status {
	case "TERMINATED", "STOPPED":
		if err := cli.StartInstance(ctx, project, t.zone, t.instance); err != nil {
			n.setGCPErr(t.id, "instances.start 失败: "+err.Error())
			log.Printf("GCP 自动开机(%s): start 失败: %v", t.name, err)
			n.fire(tgCfg, alertEvent{
				Type:       evtGCPFailed,
				ServerID:   t.id,
				ServerName: t.name,
				Text:       fmt.Sprintf("⚠️ GCP 自动开机失败\n%s 第 %d/%d 次：%v", t.name, tries, cfg.MaxTries, err),
			})
			return
		}
		n.setGCPErr(t.id, "")
		log.Printf("GCP 自动开机(%s): 已调用 instances.start（第 %d/%d 次）", t.name, tries, cfg.MaxTries)
		n.fire(tgCfg, alertEvent{
			Type:       evtGCPStarting,
			ServerID:   t.id,
			ServerName: t.name,
			Text:       fmt.Sprintf("🔄 GCP 自动开机\n%s 已调用 instances.start（第 %d/%d 次），等待节点上线", t.name, tries, cfg.MaxTries),
		})
	case "RUNNING":
		n.setGCPErr(t.id, "实例运行中但节点离线，疑似 agent/网络故障")
		n.mu.Lock()
		st, ok := n.gcp[t.id]
		fire := ok && !st.warnedRun
		if ok {
			st.warnedRun = true
		}
		n.mu.Unlock()
		if fire {
			n.fire(tgCfg, alertEvent{
				Type:       evtGCPRunningNC,
				ServerID:   t.id,
				ServerName: t.name,
				Text:       fmt.Sprintf("⚠️ GCP 守护提醒\n%s 实例状态为 RUNNING 但节点离线，可能是 agent 或网络故障，不执行开机", t.name),
			})
		}
	case "SUSPENDED":
		n.setGCPErr(t.id, "实例已挂起（SUSPENDED），暂不支持自动恢复")
		log.Printf("GCP 自动开机(%s): 实例 SUSPENDED，跳过", t.name)
	default:
		// PROVISIONING/STAGING/STOPPING/REPAIRING 等过渡态，冷却后下一轮再看
		log.Printf("GCP 自动开机(%s): 实例状态 %s，跳过本次", t.name, status)
	}
}

// ManualStartGCP 手动立即开机：忽略冷却、不消耗自动尝试次数，
// 但记录 lastTry 让自动循环退让一个冷却期，避免背靠背双重 start。
func (n *Notifier) ManualStartGCP(t gcpTarget) (status string, started bool, err error) {
	n.mu.Lock()
	st, ok := n.gcp[t.id]
	if ok && st.inFlight {
		n.mu.Unlock()
		return "", false, errGCPBusy
	}
	if !ok {
		st = &gcpState{offlineAt: time.Now()}
		n.gcp[t.id] = st
	}
	st.inFlight = true
	st.lastTry = time.Now()
	n.mu.Unlock()
	defer func() {
		n.mu.Lock()
		if st, ok := n.gcp[t.id]; ok {
			st.inFlight = false
			if err != nil {
				st.lastErr = err.Error()
			}
		}
		n.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cli, project, status, err := n.gcpResolveStatus(ctx, t)
	if err != nil {
		if cli == nil {
			return "", false, err
		}
		return "", false, fmt.Errorf("查询实例状态失败: %w", err)
	}
	if status == "TERMINATED" || status == "STOPPED" {
		if err = cli.StartInstance(ctx, project, t.zone, t.instance); err != nil {
			return status, false, err
		}
		log.Printf("GCP 手动开机(%s): 已调用 instances.start", t.instance)
		return status, true, nil
	}
	return status, false, nil
}

// ResetGCPState 丢弃某节点的自动开机运行态（尝试次数、冷却、放弃标记）。
//
// 节点的 GCP 配置被改动时调用：改了凭证或实例名，说明用户正在修复问题，
// 旧的失败计数按新配置算已经没有意义，还会让「已达上限、不再尝试」延续到新配置上。
// 节点被删除时同样调用——否则 map 里会永久留下一条已不存在节点的记录。
func (n *Notifier) ResetGCPState(id string) {
	n.mu.Lock()
	delete(n.gcp, id)
	n.mu.Unlock()
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
