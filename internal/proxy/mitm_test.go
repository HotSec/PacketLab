package proxy

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestLoadOrGenerateCA_GeneratesNew 验证首次调用生成 CA 并持久化到磁盘。
func TestLoadOrGenerateCA_GeneratesNew(t *testing.T) {
	dir := t.TempDir()
	certPEM, keyPEM, err := LoadOrGenerateCA(dir)
	if err != nil {
		t.Fatalf("LoadOrGenerateCA: %v", err)
	}
	if len(certPEM) == 0 {
		t.Fatal("expected non-empty certPEM")
	}
	if len(keyPEM) == 0 {
		t.Fatal("expected non-empty keyPEM")
	}
	// 验证文件已持久化
	if _, err := os.Stat(filepath.Join(dir, "ca.crt")); err != nil {
		t.Fatalf("ca.crt not persisted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ca.key")); err != nil {
		t.Fatalf("ca.key not persisted: %v", err)
	}
	// 验证 key 权限 0600
	info, err := os.Stat(filepath.Join(dir, "ca.key"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("expected ca.key 0600, got %v", info.Mode().Perm())
	}
}

// TestLoadOrGenerateCA_LoadsExisting 验证第二次调用加载已有证书（返回相同 PEM）。
func TestLoadOrGenerateCA_LoadsExisting(t *testing.T) {
	dir := t.TempDir()
	cert1, key1, err := LoadOrGenerateCA(dir)
	if err != nil {
		t.Fatalf("first LoadOrGenerateCA: %v", err)
	}
	cert2, key2, err := LoadOrGenerateCA(dir)
	if err != nil {
		t.Fatalf("second LoadOrGenerateCA: %v", err)
	}
	if !bytes.Equal(cert1, cert2) {
		t.Fatal("expected same certPEM on reload")
	}
	if !bytes.Equal(key1, key2) {
		t.Fatal("expected same keyPEM on reload")
	}
}
