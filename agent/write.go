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
		if task.Mode != 0 {
			if err := os.Chmod(task.Path, os.FileMode(task.Mode)); err != nil {
				return fmt.Errorf("内容已追加，但无法把权限设为 %04o（文件可能属于其他用户）: %w",
					task.Mode, err)
			}
		}
		return nil
	}

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
	defer os.Remove(tmpName)

	if _, err := tmp.Write(task.Data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	return os.Rename(tmpName, task.Path)
}
