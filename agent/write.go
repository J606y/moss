package main

import (
	"fmt"
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

func writeFile(task protocol.WriteTask) error {
	mode := os.FileMode(task.Mode)
	if mode == 0 {
		mode = 0o644
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
		return f.Close()
	}

	// 覆盖写必须是原子的：直接截断目标文件再写，中途失败（磁盘满、进程被杀）
	// 会留下一个半截的配置文件，比完全没写更危险——服务可能就此起不来。
	// 先写同目录下的临时文件，落盘后再 rename 顶替。同目录是必要条件，
	// 跨文件系统的 rename 会失败。
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
