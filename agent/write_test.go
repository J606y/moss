package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"moss/internal/protocol"
)

// requirePOSIXPerm 跳过 Windows：那里没有 POSIX 权限位，
// os.Chmod 只能改只读标志，本组用例的语义不成立。
func requirePOSIXPerm(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Windows 没有 POSIX 权限位，本用例不适用")
	}
}

// writeExisting 造一个指定权限的已有文件。
// 显式 Chmod 一次：os.WriteFile 的 perm 会被 umask 削掉。
func writeExisting(t *testing.T, name string, perm os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("old"), perm); err != nil {
		t.Fatalf("准备文件失败: %v", err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatalf("设置权限失败: %v", err)
	}
	return path
}

func permOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("读取文件信息失败: %v", err)
	}
	return fi.Mode().Perm()
}

// TestResolveOverwriteMode 覆盖写的权限决策，本组最关键的一条。
//
// 覆盖写走「临时文件 + rename 顶替」，原 inode 的权限位随之丢失。旧代码在
// mode 为 0 时一律补默认的 0644：于是一次不带 mode 的改配置，会把 0600 的
// .env / 私钥 / token 静默放宽到 0644——同机的其他用户从此可读，
// 而审计日志上只写着「写入成功」。同一个工具的 append 分支反而不会改权限。
//
// 这条不跳过 Windows：判断本身是纯逻辑，跟平台无关；
// 真正落到文件系统上的效果由下面几条 POSIX 用例覆盖。
func TestResolveOverwriteMode(t *testing.T) {
	cases := []struct {
		name     string
		taskMode uint32
		existing os.FileMode
		exists   bool
		want     os.FileMode
	}{
		{"未指定 mode，保留 0600 不放宽", 0, 0o600, true, 0o600},
		{"未指定 mode，保留 0400 不放宽", 0, 0o400, true, 0o400},
		{"未指定 mode，也不收紧已有的 0666", 0, 0o666, true, 0o666},
		{"显式 mode 覆盖原权限", 0o600, 0o644, true, 0o600},
		{"显式 mode 作用于新文件", 0o600, 0, false, 0o600},
		{"新文件无权限可继承，取默认值", 0, 0, false, 0o644},
	}
	for _, c := range cases {
		if got := resolveOverwriteMode(c.taskMode, c.existing, c.exists); got != c.want {
			t.Errorf("%s: 得到 %#o，期望 %#o", c.name, got, c.want)
		}
	}
}

// TestWriteOverwriteKeepsExistingMode 这是本组最关键的一条。
//
// 覆盖写走的是「临时文件 + rename 顶替」，原 inode 的权限位随之丢失。
// 旧代码在 mode 为 0 时一律套 0644：于是一次不带 mode 的改配置，
// 会把 0600 的 .env / 私钥 / token 文件静默放宽到 0644——同机的其他用户从此可读，
// 而审计日志上只写着「写入成功」。同一个工具的 append 分支反而不会改权限。
func TestWriteOverwriteKeepsExistingMode(t *testing.T) {
	requirePOSIXPerm(t)
	path := writeExisting(t, "secret.env", 0o600)

	if err := writeFile(protocol.WriteTask{Path: path, Data: []byte("new")}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	if got := permOf(t, path); got != 0o600 {
		t.Fatalf("未指定 mode 的覆盖写把权限从 0600 改成了 %#o，密钥文件被静默放宽", got)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "new" {
		t.Fatalf("内容应已更新，实际 %q err=%v", b, err)
	}
}

// TestWriteOverwriteKeepsWideMode 反方向同样要成立：原本就是 0666 的文件
// 不该被悄悄收紧，否则依赖它的其他进程会突然读不到。
func TestWriteOverwriteKeepsWideMode(t *testing.T) {
	requirePOSIXPerm(t)
	path := writeExisting(t, "shared.conf", 0o666)

	if err := writeFile(protocol.WriteTask{Path: path, Data: []byte("new")}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if got := permOf(t, path); got != 0o666 {
		t.Fatalf("未指定 mode 的覆盖写把权限从 0666 改成了 %#o", got)
	}
}

// TestWriteExplicitModeWins 显式指定 mode 时以调用方为准，覆盖原权限。
func TestWriteExplicitModeWins(t *testing.T) {
	requirePOSIXPerm(t)
	path := writeExisting(t, "app.conf", 0o644)

	if err := writeFile(protocol.WriteTask{Path: path, Data: []byte("new"), Mode: 0o600}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if got := permOf(t, path); got != 0o600 {
		t.Fatalf("显式 mode 应当生效，实际 %#o", got)
	}
}

// TestWriteNewFileDefaultsTo644 目标不存在时没有可保留的权限，仍用默认值。
func TestWriteNewFileDefaultsTo644(t *testing.T) {
	requirePOSIXPerm(t)
	path := filepath.Join(t.TempDir(), "brand-new.conf")

	if err := writeFile(protocol.WriteTask{Path: path, Data: []byte("hi")}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if got := permOf(t, path); got != 0o644 {
		t.Fatalf("新建文件应为 0644，实际 %#o", got)
	}
}

// TestWriteAppendKeepsExistingMode 没给 mode 就是「不关心权限」，
// 这时追加不该动它——与覆盖写的规则一致。
func TestWriteAppendKeepsExistingMode(t *testing.T) {
	requirePOSIXPerm(t)
	path := writeExisting(t, "audit.log", 0o600)

	if err := writeFile(protocol.WriteTask{Path: path, Data: []byte("+more"), Append: true}); err != nil {
		t.Fatalf("追加失败: %v", err)
	}
	if got := permOf(t, path); got != 0o600 {
		t.Fatalf("追加不应改动权限，实际 %#o", got)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "old+more" {
		t.Fatalf("应为追加而非覆盖，实际 %q err=%v", b, err)
	}
}

// TestWriteMkdirCreatesParents 顺带守住 Mkdir 语义：目标目录不存在时
// 覆盖写要先建目录，否则 CreateTemp 会失败。
func TestWriteMkdirCreatesParents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a", "b", "c.conf")

	if err := writeFile(protocol.WriteTask{Path: path, Data: []byte("hi"), Mkdir: true}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "hi" {
		t.Fatalf("内容应写入成功，实际 %q err=%v", b, err)
	}
}

// TestWriteAppendAppliesExplicitMode 显式给了 mode，追加也必须让它生效。
//
// OpenFile 的 mode 只在**创建**文件时生效。追加到一个已存在的文件时，
// 调用方要的 0600 完全不起作用：内容进去了、权限还是原来的 0644，
// 而返回的是「写入成功」——一条追加进去的密钥就这样躺在人人可读的文件里。
// 这条与覆盖写那条方向相反（该收紧却没收紧），但同属「权限与预期不符且无提示」。
func TestWriteAppendAppliesExplicitMode(t *testing.T) {
	requirePOSIXPerm(t)
	path := writeExisting(t, "secrets.env", 0o644)

	if err := writeFile(protocol.WriteTask{
		Path: path, Data: []byte("TOKEN=abc"), Append: true, Mode: 0o600,
	}); err != nil {
		t.Fatalf("追加失败: %v", err)
	}
	if got := permOf(t, path); got != 0o600 {
		t.Fatalf("显式指定的 mode 必须生效，实际 %#o —— 追加进去的密钥仍是人人可读", got)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "oldTOKEN=abc" {
		t.Fatalf("应为追加而非覆盖，实际 %q err=%v", b, err)
	}
}

// TestWriteAppendCreatesWithExplicitMode 目标不存在时，创建就该带上指定权限。
func TestWriteAppendCreatesWithExplicitMode(t *testing.T) {
	requirePOSIXPerm(t)
	path := filepath.Join(t.TempDir(), "new.env")

	if err := writeFile(protocol.WriteTask{
		Path: path, Data: []byte("A=1"), Append: true, Mode: 0o600,
	}); err != nil {
		t.Fatalf("追加失败: %v", err)
	}
	if got := permOf(t, path); got != 0o600 {
		t.Fatalf("新建文件应带上指定权限，实际 %#o", got)
	}
}
