// Package kubeconfig configures Kubernetes clients for the node Agent and the
// storage API. It never implicitly loads a user's kubeconfig.
package kubeconfig

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"k8s.io/client-go/rest"
)

func Config() (*rest.Config, error) {
	endpoint := os.Getenv("KUBE_API_URL")
	if endpoint == "" {
		host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT_HTTPS")
		if host == "" {
			host = "kubernetes.default.svc"
		}
		if port == "" {
			port = "443"
		}
		endpoint = "https://" + net.JoinHostPort(host, port)
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid Kubernetes API URL")
	}
	ip := net.ParseIP(u.Hostname())
	loopback := u.Hostname() == "localhost" || ip != nil && ip.IsLoopback()
	if u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Scheme != "https" && !(u.Scheme == "http" && loopback)) {
		return nil, fmt.Errorf("Kubernetes API must use HTTPS (HTTP allowed only for a local proxy); URL credentials, queries and fragments are unsupported")
	}
	config := &rest.Config{Host: strings.TrimRight(endpoint, "/"), Timeout: 20 * time.Second, UserAgent: "diskfleet/go"}
	for _, entry := range []struct {
		env, fallback string
		target        *string
	}{
		{"KUBE_TOKEN_FILE", "/var/run/secrets/kubernetes.io/serviceaccount/token", &config.BearerTokenFile},
		{"KUBE_CA_FILE", "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt", &config.TLSClientConfig.CAFile},
	} {
		filename := os.Getenv(entry.env)
		if filename != "" {
			*entry.target = filename
			continue
		}
		if _, err := os.Stat(entry.fallback); err == nil {
			*entry.target = entry.fallback
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read ServiceAccount credential metadata: %w", err)
		}
	}
	return config, nil
}
