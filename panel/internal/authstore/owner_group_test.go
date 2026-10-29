package authstore

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// errChownDenied 模拟非 root 进程 chown 的 EPERM。
var errChownDenied = errors.New("operation not permitted")

// chownCall 记录一次 chown 调用（uid/gid 中 -1 表示不改那一项）。
type chownCall struct {
	Path string
	UID  int
	GID  int
}

// TestOwnerGroupFailureDoesNotBlockSave 属组改不动不阻断落盘。
//
// 背景：跨 uid 部署时网关靠**属主**读文件（0600 同 uid 可读），属组只影响附加的
// 组可读性。此前是一次 chown(uid, gid)，属组失败也上抛，用户看到「凭证已写入，
// 但调整文件属主失败」并被引去查一个其实正常的目录。
//
// 非 root 环境造不出真实 EPERM（chown 自己的文件同样成功）的「属主成功、属组失败」
// 组合，故注入 chown 实现：属主放行、属组拒绝。
func TestOwnerGroupFailureDoesNotBlockSave(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	var calls []chownCall
	s.chown = func(path string, uid, gid int) error {
		calls = append(calls, chownCall{Path: path, UID: uid, GID: gid})
		if gid >= 0 {
			return errChownDenied // 属组改不动
		}
		return nil
	}
	s.SetOwner(10001, 10001)

	a := &Account{UID: "uid-gid-fail-1", AccessToken: "at", RefreshToken: "rt"}
	if err := s.Save(a); err != nil {
		t.Fatalf("属组失败不该阻断落盘，got %v", err)
	}
	// 两段式：先属主（uid, -1），再属组（-1, gid）。
	if len(calls) != 2 {
		t.Fatalf("应分两次 chown（属主 + 属组），实际 %d 次: %+v", len(calls), calls)
	}
	if calls[0].UID != 10001 || calls[0].GID != -1 {
		t.Errorf("第一次应只改属主（10001, -1），got %+v", calls[0])
	}
	if calls[1].UID != -1 || calls[1].GID != 10001 {
		t.Errorf("第二次应只改属组（-1, 10001），got %+v", calls[1])
	}
	// 凭证内容必须真的落盘（不能因为属组告警就回滚/跳过写入）。
	p := s.PathFor(a.UID)
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("凭证文件应存在: %v", err)
	}
	if !strings.Contains(string(raw), a.UID) {
		t.Errorf("凭证文件内容不对: %s", raw)
	}
}

// TestOwnerUIDFailureStillErrors 属主改不动必须上抛：跨 uid 部署时网关正是靠属主
// 读文件，静默成功会变成「账号加进去了但池里没有」，比明确报错更难查。
func TestOwnerUIDFailureStillErrors(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	var calls []chownCall
	s.chown = func(path string, uid, gid int) error {
		calls = append(calls, chownCall{Path: path, UID: uid, GID: gid})
		if uid >= 0 {
			return errChownDenied // 属主改不动
		}
		return nil
	}
	s.SetOwner(10001, 10001)

	err = s.Save(&Account{UID: "uid-uid-fail-1", AccessToken: "at", RefreshToken: "rt"})
	if err == nil {
		t.Fatal("属主失败必须返回错误（否则表现为账号已加但池里没有）")
	}
	if !strings.Contains(err.Error(), "属主") {
		t.Errorf("错误信息应指明是属主问题，got %v", err)
	}
	if !errors.Is(err, errChownDenied) {
		t.Errorf("应包装底层 chown 错误，got %v", err)
	}
	// 属主失败后不该再改属组（没有属主就没有「组可读性」的意义，也少一次系统调用）。
	for _, c := range calls {
		if c.UID < 0 && c.GID >= 0 {
			t.Errorf("属主失败后不应继续调整属组: %+v", calls)
		}
	}
}

// TestOwnerOnlyGIDConfigured 只配属组（uid=-1）时也走两段式的第二段，且失败不阻断。
func TestOwnerOnlyGIDConfigured(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.chown = func(path string, uid, gid int) error {
		if uid >= 0 {
			t.Fatalf("未配置属主时不应改属主，got uid=%d", uid)
		}
		return errChownDenied
	}
	s.SetOwner(-1, 4321)
	if err := s.Save(&Account{UID: "uid-gid-only-1", AccessToken: "at"}); err != nil {
		t.Fatalf("只配属组且改不动时不该阻断落盘，got %v", err)
	}
	if _, err := os.Stat(s.PathFor("uid-gid-only-1")); err != nil {
		t.Fatalf("凭证文件应存在: %v", err)
	}
}
