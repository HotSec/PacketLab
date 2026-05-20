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

	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject: pkix.Name{
			Organization: []string{org},
			CommonName:   org + " MITM CA",
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
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

	// 生成新证书
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

	fmt.Printf("[mitm] CA 证书已生成: %s\n", certPath)
	fmt.Printf("[mitm] 安装此证书到系统信任根以解密 HTTPS 流量\n")

	return certPEM, keyPEM, nil
}
