package proxy

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// GenerateCA 生成自签名 CA 证书，返回 PEM 编码的 cert 和 key
func GenerateCA(org string) (certPEM, keyPEM []byte, err error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, fmt.Errorf("generate key: %w", err)
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		serialNumber = big.NewInt(time.Now().UnixNano())
	}

	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{org},
			CommonName:   org + " MITM CA",
		},
		NotBefore: time.Now().Add(-24 * time.Hour),
		NotAfter:  time.Now().AddDate(10, 0, 0),
		KeyUsage:  x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("create cert: %w", err)
	}

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM = pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})

	return certPEM, keyPEM, nil
}

// LoadOrGenerateCA 加载或生成 CA 证书
func LoadOrGenerateCA(certDir string) (certPEM, keyPEM []byte, err error) {
	certPath := filepath.Join(certDir, "ca.crt")
	keyPath := filepath.Join(certDir, "ca.key")

	if _, err := os.Stat(certPath); err == nil {
		certPEM, err = os.ReadFile(certPath)
		if err != nil {
			return nil, nil, fmt.Errorf("read cert: %w", err)
		}
		keyPEM, err = os.ReadFile(keyPath)
		if err != nil {
			return nil, nil, fmt.Errorf("read key: %w", err)
		}
		return certPEM, keyPEM, nil
	}

	if err := os.MkdirAll(certDir, 0700); err != nil {
		return nil, nil, fmt.Errorf("mkdir certs: %w", err)
	}

	certPEM, keyPEM, err = GenerateCA("PacketLab")
	if err != nil {
		return nil, nil, err
	}

	if err := os.WriteFile(certPath, certPEM, 0600); err != nil {
		return nil, nil, fmt.Errorf("write cert: %w", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		return nil, nil, fmt.Errorf("write key: %w", err)
	}

	printInstallGuide(certPath)

	return certPEM, keyPEM, nil
}

func printInstallGuide(certPath string) {
	msg := `
============================================================
  CA 证书已生成，需要安装才能解密 HTTPS 流量
============================================================

  macOS 安装步骤:
  1. 双击打开证书文件:
     open ` + certPath + `
  2. 钥匙串访问 -> 选择 系统 钥匙串
  3. 双击已添加的证书 -> 展开 信任 部分
  4. 使用此证书时 -> 选择 始终信任
  5. 关闭窗口，输入密码确认

  命令行快速安装:
  sudo security add-trusted-cert -d -r trustRoot \
    -k /Library/Keychains/System.keychain \
    ` + certPath + `

  Windows:
  certutil -addstore Root ` + certPath + `

  Linux:
  sudo cp ` + certPath + ` /usr/local/share/ca-certificates/
  sudo update-ca-certificates

============================================================
`
	fmt.Print(msg)
}
