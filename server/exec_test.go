package main

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"moss/internal/protocol"
)

func TestCheckDestructiveBlocks(t *testing.T) {
	blocked := []string{
		`rm -rf /`,
		`rm -rf /*`,
		`rm -fr /`,
		`rm --no-preserve-root -rf /`,
		`rm -rf "/"`,
		`mkfs.ext4 /dev/sda1`,
		`dd if=/dev/zero of=/dev/sda bs=1M`,
		`echo x > /dev/sda`,
		`shutdown -h now`,
		`poweroff`,
		`init 0`,
		`chmod -R 777 /`,
		`chown -R nobody:nobody /`,
		`systemctl stop moss-agent`,
		`systemctl disable moss-agent`,
		// service 的参数顺序与 systemctl 相反，两种都要拦
		`service moss-agent stop`,
		`rm -f /usr/local/bin/moss-agent`,
		`wipefs -a /dev/sda`,
	}
	for _, cmd := range blocked {
		if why := checkDestructive(cmd); why == "" {
			t.Errorf("应被拦截却放行了: %q", cmd)
		}
	}
}

// TestCheckLockoutBlocks 自断手脚类：执行完 moss 就再也够不到这台机器。
func TestCheckLockoutBlocks(t *testing.T) {
	blocked := []string{
		// 防火墙写操作整类拦——一条规则会不会挡住 SSH，静态看命令判断不出来
		`iptables -A INPUT -j DROP`,
		`iptables -P INPUT DROP`,
		`iptables -A INPUT -p tcp --dport 22 -j DROP`,
		`iptables -F`,
		`ip6tables -A INPUT -j REJECT`,
		`ufw enable`,
		`ufw default deny incoming`,
		`ufw delete allow 22`,
		`firewall-cmd --remove-service=ssh --permanent`,
		`nft add rule inet filter input drop`,
		`iptables-restore < /tmp/rules`,
		// SSH 配置写入的各种形态
		`sed -i 's/Port 22/Port 2222/' /etc/ssh/sshd_config`,
		`echo "Port 2222" >> /etc/ssh/sshd_config`,
		`echo "PermitRootLogin no" > /etc/ssh/sshd_config`,
		`tee /etc/ssh/sshd_config < /tmp/new`,
		`cp /tmp/sshd_config /etc/ssh/sshd_config`,
		`rm /etc/ssh/sshd_config`,
		// 停用 SSH 服务
		`systemctl stop sshd`,
		`systemctl disable ssh`,
		`service ssh stop`,
		// 关网卡
		`ip link set eth0 down`,
		`ifdown eth0`,
	}
	for _, cmd := range blocked {
		if why := checkDestructive(cmd); why == "" {
			t.Errorf("自断手脚类命令应被拦截却放行了: %q", cmd)
		}
	}
}

// TestCheckLockoutAllowsReadOnly 防误伤：诊断问题时要能看清防火墙和 SSH 现状。
func TestCheckLockoutAllowsReadOnly(t *testing.T) {
	allowed := []string{
		`iptables -L`,
		`iptables -L -n -v`,
		`iptables -S`,
		`ip6tables -L INPUT`,
		`ufw status`,
		`ufw status verbose`,
		`firewall-cmd --list-all`,
		`firewall-cmd --state`,
		`nft list ruleset`,
		`cat /etc/ssh/sshd_config`,
		`grep Port /etc/ssh/sshd_config`,
		`grep -i permitrootlogin /etc/ssh/sshd_config`,
		`head -20 /etc/ssh/sshd_config`,
		// restart 不改配置，是改完配置后的正常生效动作
		`systemctl restart sshd`,
		`systemctl status sshd`,
		// 与 ssh 无关的日常命令不应被这组规则误伤
		`ssh-keygen -l -f /etc/ssh/ssh_host_rsa_key.pub`,
		`ip link show`,
		`ip addr show eth0`,
	}
	for _, cmd := range allowed {
		if why := checkDestructive(cmd); why != "" {
			t.Errorf("只读/无关命令被误拦: %q（原因：%s）", cmd, why)
		}
	}
}

// TestCheckDestructiveAllowsNormalCommands 防误伤。
// 黑名单拦错正常命令，等于让整个功能不可用——这比漏拦更容易发生。
func TestCheckDestructiveAllowsNormalCommands(t *testing.T) {
	allowed := []string{
		`rm -rf /tmp/build`,
		`rm -rf ./node_modules`,
		`rm -rf /var/log/moss/*.log`,
		`rm -f /opt/app/cache.db`,
		`docker compose up -d`,
		`systemctl restart nginx`,
		`systemctl status moss-agent`, // 查状态不是停用
		`journalctl -u moss-agent -n 50`,
		`df -h /`,
		`du -sh /var/log`,
		`ls -la /`,
		`tar -czf /tmp/backup.tar.gz /etc/nginx`,
		`dd if=/dev/urandom of=/tmp/testfile bs=1M count=10`, // 写普通文件，不是块设备
		`chmod 644 /etc/nginx/nginx.conf`,
		`chown -R www-data:www-data /var/www`,
		`git pull && npm run build`,
		`curl -fsSL https://example.com/install.sh | sh`,
	}
	for _, cmd := range allowed {
		if why := checkDestructive(cmd); why != "" {
			t.Errorf("正常命令被误拦: %q（原因：%s）", cmd, why)
		}
	}
}

func newTestJob(id, serverID string) *execJob {
	return &execJob{id: id, serverID: serverID, started: time.Now(), done: make(chan struct{})}
}

func TestExecJobFinishIsIdempotent(t *testing.T) {
	job := newTestJob("j1", "s1")
	job.finish(0, "", false)
	// agent 回传 Done 与 agent 掉线可能同时发生，二次收敛不能 panic（重复 close channel）。
	job.finish(1, "掉线", false)

	out := job.outcome()
	if out.ExitCode != 0 || out.Error != "" {
		t.Fatalf("首次收敛的结果应被保留，实际 %+v", out)
	}
}

func TestExecJobOutcomeReordersChunks(t *testing.T) {
	job := newTestJob("j2", "s1")
	// 乱序投递，且 stdout / stderr 交错
	job.addChunk(execChunk{seq: 2, stream: "stdout", data: []byte("C")})
	job.addChunk(execChunk{seq: 0, stream: "stdout", data: []byte("A")})
	job.addChunk(execChunk{seq: 3, stream: "stderr", data: []byte("y")})
	job.addChunk(execChunk{seq: 1, stream: "stderr", data: []byte("x")})
	job.finish(0, "", false)

	out := job.outcome()
	if out.Stdout != "AC" {
		t.Errorf("stdout 应按 seq 还原为 AC，实际 %q", out.Stdout)
	}
	if out.Stderr != "xy" {
		t.Errorf("stderr 应按 seq 还原为 xy，实际 %q", out.Stderr)
	}
}

func TestExecJobIgnoresChunksAfterFinish(t *testing.T) {
	job := newTestJob("j3", "s1")
	job.addChunk(execChunk{seq: 0, stream: "stdout", data: []byte("A")})
	job.finish(0, "", false)
	job.addChunk(execChunk{seq: 1, stream: "stdout", data: []byte("late")})

	if out := job.outcome(); out.Stdout != "A" {
		t.Fatalf("收敛后到达的分片应被丢弃，实际 %q", out.Stdout)
	}
}

// TestOnAgentGoneConvergesPendingJobs 是本模块最关键的一条：
// agent 掉线时在途任务必须立即收敛，否则调用方会一直等到宽限超时。
func TestOnAgentGoneConvergesPendingJobs(t *testing.T) {
	m := newExecManager(testDB(t))
	target := newTestJob("j4", "s1")
	other := newTestJob("j5", "s2")
	m.jobs["j4"] = target
	m.jobs["j5"] = other

	m.OnAgentGone("s1")

	select {
	case <-target.done:
	case <-time.After(2 * time.Second):
		t.Fatal("掉线 agent 名下的任务未被收敛，调用方会一直挂起")
	}
	if out := target.outcome(); out.Error == "" {
		t.Fatal("收敛结果必须带错误说明")
	}

	select {
	case <-other.done:
		t.Fatal("其它服务器的任务不应被牵连收敛")
	default:
	}
}

func TestOnResultAggregatesAndFinishes(t *testing.T) {
	m := newExecManager(testDB(t))
	job := newTestJob("j6", "s1")
	m.jobs["j6"] = job

	m.OnResult(&protocol.ExecResult{ID: "j6", Seq: 0, Stream: "stdout", Data: []byte("hello")})
	m.OnResult(&protocol.ExecResult{ID: "j6", Seq: 1, Done: true, ExitCode: 7})

	select {
	case <-job.done:
	case <-time.After(2 * time.Second):
		t.Fatal("收到 Done 分片后任务应收敛")
	}
	out := job.outcome()
	if out.Stdout != "hello" || out.ExitCode != 7 {
		t.Fatalf("聚合结果不符，实际 %+v", out)
	}
}

func TestOnResultIgnoresUnknownJob(t *testing.T) {
	m := newExecManager(testDB(t))
	// 服务端重启后迟到的分片没有归属，必须安静丢弃而不是 panic。
	m.OnResult(&protocol.ExecResult{ID: "ghost", Seq: 0, Done: true})
}

// TestAuditWrittenBeforeExecution 验证审计在下发前落库：
// 服务端若在执行途中崩溃，仍须留有「曾经下发过什么」的痕迹。
func TestAuditWrittenBeforeExecution(t *testing.T) {
	db := testDB(t)
	m := newExecManager(db)
	job := newTestJob("j7", "s1")

	m.auditStart(job, "s1", "admin", protocol.ExecTask{Cmd: "echo hi", Dir: "/tmp", Timeout: 30})

	var cmd string
	var finishedAt int64
	if err := db.QueryRow(`SELECT cmd, finished_at FROM exec_audit WHERE job_id = ?`, "j7").
		Scan(&cmd, &finishedAt); err != nil {
		t.Fatalf("下发前审计记录应已存在: %v", err)
	}
	if cmd != "echo hi" {
		t.Errorf("审计应记录原始命令，实际 %q", cmd)
	}
	if finishedAt != 0 {
		t.Errorf("尚未结束的执行 finished_at 应为 0，实际 %d", finishedAt)
	}

	m.auditFinish("j7", ExecOutcome{JobID: "j7", ExitCode: 3, Stdout: "hi"})
	var exitCode int
	var stdout string
	if err := db.QueryRow(`SELECT exit_code, stdout, finished_at FROM exec_audit WHERE job_id = ?`, "j7").
		Scan(&exitCode, &stdout, &finishedAt); err != nil {
		t.Fatalf("查询审计失败: %v", err)
	}
	if exitCode != 3 || stdout != "hi" || finishedAt == 0 {
		t.Errorf("审计未被正确更新: exit=%d stdout=%q finished=%d", exitCode, stdout, finishedAt)
	}
}

func TestClipForAuditMarksTruncation(t *testing.T) {
	long := make([]byte, execAuditOutputCap+100)
	for i := range long {
		long[i] = 'a'
	}
	got := clipForAudit(string(long))
	if len(got) <= execAuditOutputCap {
		t.Fatal("截断后应追加标注，长度不应仍等于上限")
	}
	if got[:execAuditOutputCap] != string(long[:execAuditOutputCap]) {
		t.Error("截断应保留头部原文")
	}
	short := "fine"
	if clipForAudit(short) != short {
		t.Error("未超限的文本不应被改动")
	}
}

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := openDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
