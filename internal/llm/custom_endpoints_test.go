package llm

import "testing"

func TestRegisterAndMatchCustomEndpoint(t *testing.T) {
	resetCustomEndpointsForTest()
	defer resetCustomEndpointsForTest()

	// 使用 /v1/chat 路径（不含 /completions），避免触发内置 OpenAI path 子串匹配
	RegisterCustomEndpoint(CustomEndpoint{Host: "api.deepseek.com", Path: "/v1/chat", Name: "DeepSeek"})

	tests := []struct {
		name     string
		host     string
		path     string
		expected bool
	}{
		{"exact host match", "api.deepseek.com", "/v1/chat", true},
		{"subdomain match", "proxy.api.deepseek.com", "/v1/chat", true},
		{"path prefix match", "api.deepseek.com", "/v1/chat/extra", true},
		{"wrong host", "api.other.com", "/v1/chat", false},
		{"wrong path", "api.deepseek.com", "/v1/other", false},
		{"empty path endpoint matches any", "api.test.com", "/anything", true},
	}

	// 加一个 empty-path endpoint
	RegisterCustomEndpoint(CustomEndpoint{Host: "api.test.com", Path: "", Name: "TestAny"})

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ok := matchCustomEndpoint(tt.host, tt.path)
			if ok != tt.expected {
				t.Errorf("matchCustomEndpoint(%q, %q) = %v, want %v", tt.host, tt.path, ok, tt.expected)
			}
		})
	}
}

func TestDetectProvider_CustomEndpoint(t *testing.T) {
	resetCustomEndpointsForTest()
	defer resetCustomEndpointsForTest()

	// 使用非内置厂商 host：内置表未命中时才查自定义端点注册表
	const host = "api.custom-llm.com"

	// 未注册时 unknown
	if p := DetectProvider(host, "/v1/chat"); p != ProviderUnknown {
		t.Fatalf("before register: expected unknown, got %v", p)
	}

	RegisterCustomEndpoint(CustomEndpoint{Host: host, Path: "/v1/chat"})

	// 注册后识别为 openai（OpenAI 兼容）
	if p := DetectProvider(host, "/v1/chat"); p != ProviderOpenAI {
		t.Fatalf("after register: expected openai, got %v", p)
	}

	// 子域也应匹配
	if p := DetectProvider("proxy."+host, "/v1/chat"); p != ProviderOpenAI {
		t.Fatalf("subdomain: expected openai, got %v", p)
	}
}

func TestUnregisterCustomEndpoint(t *testing.T) {
	resetCustomEndpointsForTest()
	defer resetCustomEndpointsForTest()

	RegisterCustomEndpoint(CustomEndpoint{Host: "api.test.com", Path: "/v1/chat"})
	if p := DetectProvider("api.test.com", "/v1/chat"); p != ProviderOpenAI {
		t.Fatalf("before unregister: expected openai, got %v", p)
	}

	UnregisterCustomEndpoint("api.test.com", "/v1/chat")
	if p := DetectProvider("api.test.com", "/v1/chat"); p != ProviderUnknown {
		t.Fatalf("after unregister: expected unknown, got %v", p)
	}
}

func TestListCustomEndpoints(t *testing.T) {
	resetCustomEndpointsForTest()
	defer resetCustomEndpointsForTest()

	if eps := ListCustomEndpoints(); len(eps) != 0 {
		t.Fatalf("expected empty, got %d", len(eps))
	}

	RegisterCustomEndpoint(CustomEndpoint{Host: "a.com", Path: "/x"})
	RegisterCustomEndpoint(CustomEndpoint{Host: "b.com", Path: "/y"})

	eps := ListCustomEndpoints()
	if len(eps) != 2 {
		t.Fatalf("expected 2 endpoints, got %d", len(eps))
	}
}
