package kubeconfig

import "testing"

func TestExplicitConfigAndTransport(t *testing.T) {
	t.Setenv("KUBE_TOKEN_FILE", "/explicit/token")
	t.Setenv("KUBE_CA_FILE", "/explicit/ca")
	t.Setenv("KUBECONFIG", "/must/not/be/read")
	for _, test := range []struct {
		url   string
		valid bool
	}{
		{"https://kubernetes.example:6443", true},
		{"http://127.0.0.1:8001", true},
		{"http://[::1]:8001", true},
		{"http://localhost:8001", true},
		{"http://worker-01:8001", false},
		{"https://user:secret@kubernetes.example", false},
		{"https://kubernetes.example?token=secret", false},
		{"file:///tmp/kube", false},
	} {
		t.Run(test.url, func(t *testing.T) {
			t.Setenv("KUBE_API_URL", test.url)
			config, err := Config()
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v error=%v", test.valid, err)
			}
			if err == nil && (config.BearerTokenFile != "/explicit/token" || config.CAFile != "/explicit/ca" || config.Insecure) {
				t.Fatal("explicit credentials or TLS validation changed")
			}
		})
	}
}

func TestServiceEndpoint(t *testing.T) {
	t.Setenv("KUBE_API_URL", "")
	t.Setenv("KUBERNETES_SERVICE_HOST", "fd00::1")
	t.Setenv("KUBERNETES_SERVICE_PORT_HTTPS", "6443")
	config, err := Config()
	if err != nil || config.Host != "https://[fd00::1]:6443" {
		t.Fatal(config, err)
	}
}
