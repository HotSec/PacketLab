package capture

import (
	"net"
	"os"
	"testing"

	"packetlab/internal/models"
	"packetlab/internal/store"
)

type testHub struct {
	captured []*models.CapturedRequest
}

func (h *testHub) BroadcastCapture(req *models.CapturedRequest) {
	h.captured = append(h.captured, req)
}

func TestNICDataFlowE2E(t *testing.T) {
	dbPath := "/tmp/test_nic_e2e.db"
	os.Remove(dbPath)
	st, err := store.New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	hub := &testHub{}

	e := &Engine{
		store:     st,
		hub:       hub,
		procCache: make(map[string]*models.ProcessInfo),
	}

	e.ringBuf = NewMemRingBuffer(1024)
	e.writer = NewAsyncWriterPool(st, e.ringBuf, 1, 10*1e6)
	e.writer.engine = e
	e.writer.Start()

	pool := NewTCPStreamPool(e)
	stream := pool.New(net.ParseIP("192.168.1.100"), 54321, 80)

	httpReq := []byte("GET /api/test HTTP/1.1\r\nHost: example.com\r\nUser-Agent: curl/7.88\r\nAccept: */*\r\n\r\n")
	httpResp := []byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 5\r\n\r\nhello")

	stream.Feed(httpReq, true)
	stream.Feed(httpResp, false)

	e.writer.Stop()

	items, total, err := st.List("", "", "", false, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total == 0 {
		t.Fatal("expected at least 1 request in DB, got 0")
	}

	for _, item := range items {
		req, err := st.Get(item.ID)
		if err != nil {
			t.Fatalf("Get(%d) error: %v", item.ID, err)
		}
		t.Logf("ID=%d Method=%s URL=%s Status=%d", req.ID, req.Method, req.URL, req.StatusCode)
		t.Logf("  ReqHeaders: %v", req.ReqHeaders)
		t.Logf("  ReqBody: %q", req.ReqBody)
		t.Logf("  ResHeaders: %v", req.ResHeaders)
		t.Logf("  ResBody: %q", req.ResBody)

		if len(req.ReqHeaders) == 0 {
			t.Error("ReqHeaders is empty!")
		}
		if req.ResBody == "" {
			t.Error("ResBody is empty!")
		}
		if len(req.ResHeaders) == 0 {
			t.Error("ResHeaders is empty!")
		}
		if req.StatusCode != 200 {
			t.Errorf("StatusCode = %d, want 200", req.StatusCode)
		}
	}

	if len(hub.captured) == 0 {
		t.Fatal("expected hub to receive broadcast, got 0")
	}
	for _, r := range hub.captured {
		t.Logf("Broadcast: ID=%d Method=%s URL=%s", r.ID, r.Method, r.URL)
		if r.ID == 0 {
			t.Error("Broadcast request has ID=0, frontend cannot load detail!")
		}
	}
}

func TestExtractSNI(t *testing.T) {
	sni := extractSNI(tlsClientHelloExample)
	if sni != "example.com" {
		t.Errorf("extractSNI = %q, want %q", sni, "example.com")
	}

	sni = extractSNI([]byte{0x16, 0x03, 0x01})
	if sni != "" {
		t.Errorf("extractSNI on short data = %q, want empty", sni)
	}

	sni = extractSNI([]byte("GET / HTTP/1.1\r\n"))
	if sni != "" {
		t.Errorf("extractSNI on HTTP data = %q, want empty", sni)
	}
}

func TestIsTLSClientHello(t *testing.T) {
	if !isTLSClientHello(tlsClientHelloExample) {
		t.Error("isTLSClientHello should return true for TLS ClientHello")
	}
	if isTLSClientHello([]byte("GET / HTTP/1.1\r\n")) {
		t.Error("isTLSClientHello should return false for HTTP data")
	}
	if isTLSClientHello([]byte{0x16, 0x03, 0x01}) {
		t.Error("isTLSClientHello should return false for too short data")
	}
}

func TestTLSStreamEmitSNI(t *testing.T) {
	dbPath := "/tmp/test_tls_sni.db"
	os.Remove(dbPath)
	st, err := store.New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	hub := &testHub{}
	e := &Engine{
		store:     st,
		hub:       hub,
		procCache: make(map[string]*models.ProcessInfo),
	}
	e.ringBuf = NewMemRingBuffer(1024)
	e.writer = NewAsyncWriterPool(st, e.ringBuf, 1, 10*1e6)
	e.writer.engine = e
	e.writer.Start()

	pool := NewTCPStreamPool(e)
	stream := pool.New(net.ParseIP("192.168.1.100"), 54321, 443)

	stream.Feed(tlsClientHelloExample, true)

	e.writer.Stop()

	items, total, err := st.List("", "", "", false, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total == 0 {
		t.Fatal("expected at least 1 TLS record in DB, got 0")
	}

	req, _ := st.Get(items[0].ID)
	t.Logf("TLS Record: Method=%s URL=%s Host=%s IsHTTPS=%v", req.Method, req.URL, req.Host, req.IsHTTPS)
	if req.Method != "TLS" {
		t.Errorf("Method = %q, want %q", req.Method, "TLS")
	}
	if req.Host != "example.com" {
		t.Errorf("Host = %q, want %q", req.Host, "example.com")
	}
	if !req.IsHTTPS {
		t.Error("IsHTTPS should be true")
	}
	if req.ReqHeaders["TLS-SNI"] != "example.com" {
		t.Errorf("TLS-SNI header = %q, want %q", req.ReqHeaders["TLS-SNI"], "example.com")
	}
}

// Minimal TLS 1.2 ClientHello with SNI "example.com"
var tlsClientHelloExample = []byte{
	0x16, 0x03, 0x01, 0x00, 0x6d, // TLS record header
	0x01, 0x00, 0x00, 0x69, // Handshake: ClientHello, length
	0x03, 0x03, // Client version: TLS 1.2
	0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, // Random (32 bytes)
	0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f,
	0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17,
	0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f,
	0x00, // Session ID length: 0
	0x00, 0x02, 0x00, 0x2f, // Cipher suites: TLS_RSA_WITH_AES_128_CBC_SHA
	0x01, 0x00, // Compression methods: null
	0x00, 0x3a, // Extensions length: 58
	0x00, 0x00, 0x00, 0x36, // Extension: SNI, length 54
	0x00, 0x34, // SNI list length: 52
	0x00, // SNI type: hostname
	0x00, 0x0b, // Hostname length: 11
	'e', 'x', 'a', 'm', 'p', 'l', 'e', '.', 'c', 'o', 'm', // "example.com"
	0x00, 0x0d, 0x00, 0x00, 0x00, 0x00, // Extension: signature_algorithms (empty)
	0x00, 0x0b, 0x00, 0x02, 0x01, 0x00, // Extension: ec_point_formats
	0x00, 0x0a, 0x00, 0x02, 0x00, 0x01, // Extension: supported_groups
	0x00, 0x17, 0x00, 0x00, // Extension: extended_master_secret
	0xff, 0x01, 0x00, 0x01, 0x00, // Extension: renegotiation_info
}
