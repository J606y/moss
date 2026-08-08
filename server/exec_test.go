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

// TestCheckCommandBlocksChained 链式绕过。
//
// 这一组全部是审查时实测能放行的输入：只读白名单当年没有结尾锚点，
// 命令**以**一条查询开头就整串被判为只读。单条命令的用例覆盖不到这一类，
// 所以这个洞在 CI 上曾经完全不可见。
func TestCheckCommandBlocksChained(t *testing.T) {
	blocked := []string{
		`iptables -L; iptables -F`,
		`iptables -L -n && iptables -P INPUT DROP`,
		`nft list ruleset; nft flush ruleset`,
		`ufw status | grep x; ufw disable`,
		`sudo iptables -S; sudo iptables -P INPUT DROP`,
		"iptables -L\niptables -A INPUT -j DROP", // 换行与分号等价
		`cat /etc/ssh/sshd_config; echo "Port 2222" >> /etc/ssh/sshd_config`,
		`echo $(rm -rf /)`, // 子命令要单独成段
		"echo `rm -rf /`",  // 反引号同理
		`true && shutdown -h now`,
		`df -h; systemctl stop sshd`,
	}
	for _, cmd := range blocked {
		if why := checkDestructive(cmd); why == "" {
			t.Errorf("链式命令应被拦截却放行了: %q", cmd)
		}
	}
}

// TestCheckCommandBlocksQuoted 引号绕过。
//
// 规则里的 `/` 曾经写死成裸字符，给目标路径加一对引号即绕过。
// shell 里 of="/dev/sda" 与 of=/dev/sda 语义完全相同，属零成本绕过。
func TestCheckCommandBlocksQuoted(t *testing.T) {
	blocked := []string{
		`dd if=/dev/zero of="/dev/sda" bs=1M`,
		`dd if=/dev/zero of='/dev/nvme0n1'`,
		`cat /tmp/x > "/dev/sda"`,
		`wipefs -a "/dev/sda"`,
		`rm -rf --no-preserve-root ///`, // 斜杠后是斜杠，不属 (\s|$|\*)
		`rm -rf "/"`,
		`shred -n 1 "/dev/sda"`,
	}
	for _, cmd := range blocked {
		if why := checkDestructive(cmd); why == "" {
			t.Errorf("带引号/重复斜杠的命令应被拦截却放行了: %q", cmd)
		}
	}
}

// TestCheckCommandGuardsWorkingDir 工作目录参与判定。
//
// `rm -rf *` 本身完全无害，落在 `/` 才是致命的。Dir 一路直达 agent 的
// cmd.Dir，不过闸就是后门；而「cd 的目标写错」正是模块头注释点名要拦的手滑。
func TestCheckCommandGuardsWorkingDir(t *testing.T) {
	blocked := []struct{ cmd, dir string }{
		{`rm -rf *`, `/`},
		{`rm -rf .`, `/`},
		{`rm -rf ./`, `/`},
		{`find . -delete`, `/`},
		{`find . -type f -exec rm {} ;`, `/`},
		{`chmod -R 777 *`, `/`},
		{`chown -R nobody:nobody .`, `/`},
		{`rm -rf *`, `/etc`},
		{`rm -rf *`, `/usr/lib`},
		{`cd / && rm -rf *`, ``},        // cd 改变落点
		{`cd /etc; rm -rf *`, `/tmp`},   // cd 覆盖初始 dir
		{`cd ..; rm -rf *`, `/etc/ssh`}, // 相对 cd
	}
	for _, c := range blocked {
		if why := checkCommand(c.cmd, c.dir); why == "" {
			t.Errorf("危险工作目录下的批量操作应被拦截却放行了: cmd=%q dir=%q", c.cmd, c.dir)
		}
	}
}

// TestCheckCommandAllowsSafeWorkingDir 防误伤：这一组比上一组更重要。
// 拦错正常命令等于让整个 exec 功能不可用，而误伤比漏拦更容易发生。
func TestCheckCommandAllowsSafeWorkingDir(t *testing.T) {
	allowed := []struct{ cmd, dir string }{
		{`rm -rf *`, `/tmp/build`},             // 正常的构建清理
		{`rm -rf *`, `/var/log/myapp`},         // 正常的日志清理
		{`cd /tmp/build && rm -rf *`, ``},      // 同上，经 cd
		{`rm -rf ./node_modules`, `/`},         // 指名道姓，落点确定
		{`rm -rf build`, `/etc`},               // 同上
		{`chmod 644 nginx.conf`, `/etc/nginx`}, // 单文件、非递归
		{`ls -la`, `/`},
		{`df -h .`, `/`},
		{`tar -czf /tmp/etc.tar.gz .`, `/etc`}, // 只读打包
		{`git pull && npm run build`, `/opt/app`},
		{`rm -rf *`, ``}, // 没给 dir 就无从判断落点，不猜
	}
	for _, c := range allowed {
		if why := checkCommand(c.cmd, c.dir); why != "" {
			t.Errorf("正常命令被误拦: cmd=%q dir=%q（原因：%s）", c.cmd, c.dir, why)
		}
	}
}

// TestCheckCommandAllowsChainedReadOnly 切段带来的误伤面必须守住。
func TestCheckCommandAllowsChainedReadOnly(t *testing.T) {
	allowed := []string{
		`iptables -L && iptables -S`,
		`ufw status; ufw status verbose`,
		`grep Port /etc/ssh/sshd_config | wc -l`, // 切段前这条会被误拦
		`cat /etc/ssh/sshd_config | grep -i port`,
		`systemctl status sshd && journalctl -u sshd -n 20`,
		`grep halt /var/log/syslog`,    // halt 是常见英文词，不该误伤
		`systemctl status halt.target`, // 同上
		`docker compose up -d && docker ps`,
		`curl -fsSL https://example.com/x.sh | sh`,
	}
	for _, cmd := range allowed {
		if why := checkDestructive(cmd); why != "" {
			t.Errorf("只读/无关的链式命令被误拦: %q（原因：%s）", cmd, why)
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

	m.OnResult("s1", &protocol.ExecResult{ID: "j6", Seq: 0, Stream: "stdout", Data: []byte("hello")})
	m.OnResult("s1", &protocol.ExecResult{ID: "j6", Seq: 1, Done: true, ExitCode: 7})

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
	m.OnResult("s1", &protocol.ExecResult{ID: "ghost", Seq: 0, Done: true})
}

// TestAuditWrittenBeforeExecution 验证审计在下发前落库：
// 服务端若在执行途中崩溃，仍须留有「曾经下发过什么」的痕迹。
func TestAuditWrittenBeforeExecution(t *testing.T) {
	db := testDB(t)
	m := newExecManager(db)
	job := newTestJob("j7", "s1")

	_ = m.auditStart(job, "s1", "admin", protocol.ExecTask{Cmd: "echo hi", Dir: "/tmp", Timeout: 30})

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

// TestRememberIsAtomicWithUnregister 结果落袋与摘除必须是同一个原子动作。
//
// 此前摘除由 await 的 defer 先执行、remember 在其后，两次加锁之间存在一个窗口：
// jobs 里已经没有、finished 里还没有，Result 返回 found=false，
// get_result 于是回「jobId 有误，或结果已超过 30 分钟保留期」这种措辞很确定的
// 错误——而命令其实刚刚成功执行完。轮询的模型据此放弃，或者把一条非幂等的
// 命令重跑一遍。窗口很窄，但高频轮询必然命中。
func TestRememberIsAtomicWithUnregister(t *testing.T) {
	m := newExecManager(testDB(t))
	job := newTestJob("atomic1", "s1")
	m.jobs["atomic1"] = job

	// 一个持续轮询的调用方：任何一拍都不允许查不到这个 jobID。
	stop := make(chan struct{})
	missed := make(chan struct{}, 1)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, _, _, found := m.Result("atomic1"); !found {
				select {
				case missed <- struct{}{}:
				default:
				}
				return
			}
		}
	}()

	// 反复做「收敛 → 落袋」的状态转移，给窗口足够多的命中机会
	for i := 0; i < 2000; i++ {
		m.mu.Lock()
		m.jobs["atomic1"] = job
		delete(m.finished, "atomic1")
		m.mu.Unlock()
		m.remember("atomic1", "s1", ExecOutcome{JobID: "atomic1", ExitCode: 0})
	}
	close(stop)

	select {
	case <-missed:
		t.Fatal("存在 jobs 与 finished 都查不到的窗口：已完成的任务会被谎报成「jobId 有误」")
	default:
	}

	// 转移完成后应当查得到、且不再是 running
	out, _, running, found := m.Result("atomic1")
	if !found || running {
		t.Fatalf("落袋后应能查到且不再是 running，实际 found=%v running=%v", found, running)
	}
	if out.JobID != "atomic1" {
		t.Fatalf("取回的结果不对: %+v", out)
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
