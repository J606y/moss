package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"

	"moss/internal/protocol"
)

// HandleWrite 受理文件写入任务，异步执行。与命令执行共用准入闸：
// 写文件同样是改变机器状态的操作，不该有独立的开关语义。
func (r *execRunner) HandleWrite(c sender, task protocol.WriteTask) {
	if !r.allow {
		sendExecFinal(c, task.ID, 0, "本机未开启远程执行（需 --allow-exec）", false)
		return
	}
	if task.ID == "" || task.Path == "" {
		sendExecFinal(c, task.ID, 0, "任务 ID 或路径为空", false)
		return
	}
	if len(task.Data) > protocol.WriteSizeCap {
		sendExecFinal(c, task.ID, 0,
			fmt.Sprintf("内容超过上限 %d 字节，请改用命令在目标机拉取", protocol.WriteSizeCap), false)
		return
	}
	if reason := r.admit(task.ID); reason != "" {
		sendExecFinal(c, task.ID, 0, reason, false)
		return
	}
	go func() {
		defer r.done()
		if err := writeFile(task); err != nil {
			sendExecFinal(c, task.ID, 0, "写入失败: "+err.Error(), false)
			return
		}
		sendExecFinal(c, task.ID, 0, "", false)
	}()
}

// HandleRead 受理分块文件读取/下载请求，异步执行。
//
// 单帧读取字节数上限收敛至 protocol.ExecChunkSize（32KB），
// 确保经 JSON base64 编码后不会超过 WebSocket 单条消息 64KB 上限。
func (r *execRunner) HandleRead(c sender, task protocol.ReadTask) {
	go func() {
		res := r.readFileChunk(task)
		if err := c.send(protocol.AgentMsg{Type: "read_result", Read: &res}); err != nil {
			log.Printf("回传文件读取结果失败: %v", err)
		}
	}()
}

func (r *execRunner) readFileChunk(task protocol.ReadTask) protocol.ReadResult {
	if !r.allow {
		return protocol.ReadResult{
			ReqID: task.ReqID,
			Path:  task.Path,
			Error: "本机未开启远程执行（需 --allow-exec）",
		}
	}
	if task.Path == "" {
		return protocol.ReadResult{
			ReqID: task.ReqID,
			Error: "文件路径为空",
		}
	}
	fi, err := os.Stat(task.Path)
	if err != nil {
		return protocol.ReadResult{
			ReqID: task.ReqID,
			Path:  task.Path,
			Error: "读取文件失败: " + err.Error(),
		}
	}
	if fi.IsDir() {
		return protocol.ReadResult{
			ReqID: task.ReqID,
			Path:  task.Path,
			Error: "目标路径是目录而非普通文件",
		}
	}

	totalBytes := fi.Size()
	start := task.Offset
	if start < 0 {
		start = 0
	}
	if start > totalBytes {
		start = totalBytes
	}
	limit := task.Length
	if limit <= 0 || limit > protocol.ExecChunkSize {
		limit = protocol.ExecChunkSize
	}
	remain := totalBytes - start
	if remain <= 0 {
		return protocol.ReadResult{
			ReqID:      task.ReqID,
			Path:       task.Path,
			Offset:     start,
			NextOffset: start,
			TotalBytes: totalBytes,
			EOF:        true,
		}
	}
	toRead := int64(limit)
	if remain < toRead {
		toRead = remain
	}
	f, err := os.Open(task.Path)
	if err != nil {
		return protocol.ReadResult{
			ReqID: task.ReqID,
			Path:  task.Path,
			Error: "打开文件失败: " + err.Error(),
		}
	}
	defer f.Close()

	buf := make([]byte, toRead)
	n, err := f.ReadAt(buf, start)
	if err != nil && !errors.Is(err, io.EOF) && n == 0 {
		return protocol.ReadResult{
			ReqID: task.ReqID,
			Path:  task.Path,
			Error: "读取文件内容失败: " + err.Error(),
		}
	}
	buf = buf[:n]
	next := start + int64(n)
	return protocol.ReadResult{
		ReqID:      task.ReqID,
		Path:       task.Path,
		Data:       buf,
		Offset:     start,
		NextOffset: next,
		TotalBytes: totalBytes,
		EOF:        next >= totalBytes,
	}
}

// writeDefaultMode 目标文件不存在、调用方也没指定权限时用的默认值。
const writeDefaultMode os.FileMode = 0o644

// resolveOverwriteMode 决定覆盖写落地时用什么权限。
//
// 规则只有一条：调用方显式给了 mode 就听调用方的；没给就保留目标文件现有的权限；
// 目标不存在时才用默认值。
//
// 为什么必须保留：覆盖写是「换掉内容」而不是「重建文件」，调用方没提权限就不该
// 悄悄改权限。但实现上走的是「临时文件 + rename 顶替」，原 inode 连同它的权限位
// 一起被换掉了，不显式续上就等于丢失。旧代码在这里补的是默认的 0644，于是一次
// 不带 mode 的改配置，会把 0600 的 .env / 私钥 / token 静默放宽到 0644——
// 同机的其他用户从此可读，而审计日志上只写着「写入成功」。
//
// 抽成纯函数是为了能在所有平台上测：Windows 的文件权限只有「可写 / 只读」两态，
// 端到端的用例在那里必须跳过，但这条判断本身不该跟着一起失去守护。
func resolveOverwriteMode(taskMode uint32, existing os.FileMode, exists bool) os.FileMode {
	if taskMode != 0 {
		return os.FileMode(taskMode)
	}
	if exists {
		return existing.Perm()
	}
	return writeDefaultMode
}

func writeFile(task protocol.WriteTask) error {
	mode := os.FileMode(task.Mode)
	if mode == 0 {
		mode = writeDefaultMode
	}
	if task.Mkdir {
		if err := os.MkdirAll(filepath.Dir(task.Path), 0o755); err != nil {
			return err
		}
	}

	if task.Append {
		f, err := os.OpenFile(task.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, mode)
		if err != nil {
			return err
		}
		if _, err := f.Write(task.Data); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		// OpenFile 的 mode 只在**创建**文件时生效。追加到一个已存在的文件时，
		// 调用方要的 0600 完全不起作用：内容进去了，权限还是原来的 0644，
		// 而返回的是「写入成功」——一条追加进去的密钥就这样躺在人人可读的文件里。
		//
		// 只在调用方显式给了 mode 时才动权限：没给就是「不关心」，
		// 这时保持原样才对（与覆盖写的规则一致）。
		// chmod 失败一律返回错误，不静默降级——调用方明确要求了这个权限，
		// 给不了就该让他知道，而不是让他以为拿到了。
		if task.Mode != 0 {
			if err := os.Chmod(task.Path, os.FileMode(task.Mode)); err != nil {
				return fmt.Errorf("内容已追加，但无法把权限设为 %04o（文件可能属于其他用户）: %w",
					task.Mode, err)
			}
		}
		return nil
	}

	// 覆盖写必须是原子的：直接截断目标文件再写，中途失败（磁盘满、进程被杀）
	// 会留下一个半截的配置文件，比完全没写更危险——服务可能就此起不来。
	// 先写同目录下的临时文件，落盘后再 rename 顶替。同目录是必要条件，
	// 跨文件系统的 rename 会失败。
	//
	// 顶替会丢掉原 inode 的权限位，所以要先问清楚该落什么权限，见 resolveOverwriteMode。
	// append 分支没有这个问题：它写的是原 inode，OpenFile 的 mode 只在创建时生效。
	var existing os.FileMode
	var exists bool
	if fi, err := os.Stat(task.Path); err == nil && fi.Mode().IsRegular() {
		existing, exists = fi.Mode().Perm(), true
	}
	mode = resolveOverwriteMode(task.Mode, existing, exists)

	dir := filepath.Dir(task.Path)
	tmp, err := os.CreateTemp(dir, ".moss-write-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // rename 成功后此路径已不存在，删除失败无妨

	if _, err := tmp.Write(task.Data); err != nil {
		tmp.Close()
		return err
	}
	// 显式落盘：不 fsync 的话，rename 之后遭遇断电可能得到一个长度正确但内容为空的文件。
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// CreateTemp 建出来的是 0600，改成目标权限后再顶替。
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	return os.Rename(tmpName, task.Path)
}
