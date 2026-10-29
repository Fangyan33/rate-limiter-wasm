package models

import (
	"fmt"
	"strconv"
	"strings"
)

// ValidateService accepts exactly namespace/name and a Kubernetes Service port.
func ValidateService(service string, port int) error {
	parts := strings.Split(service, "/")
	if len(parts) != 2 || !validLabel(parts[0]) || !validLabel(parts[1]) {
		return fmt.Errorf("service must be a lowercase namespace/name")
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("port must be an integer between 1 and 65535")
	}
	return nil
}

func validLabel(s string) bool {
	if len(s) == 0 || len(s) > 63 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for _, c := range s {
		if c != '-' && (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

func ValidClusterDomain(domain string) bool {
	if len(domain) == 0 || len(domain) > 253 {
		return false
	}
	for _, label := range strings.Split(domain, ".") {
		if !validLabel(label) {
			return false
		}
	}
	return true
}

// ParseUpstreamCluster only recognizes Istio's Kubernetes outbound clusters.
// Subsets share the service-port quota. No DNS or Kubernetes calls are made.
func ParseUpstreamCluster(cluster, clusterDomain string) (service string, port int, ok bool) {
	if len(cluster) == 0 || len(cluster) > 1024 || !ValidClusterDomain(clusterDomain) {
		return "", 0, false
	}
	parts := strings.Split(cluster, "|")
	if len(parts) != 4 || parts[0] != "outbound" {
		return "", 0, false
	}
	port, err := strconv.Atoi(parts[1])
	if err != nil || strconv.Itoa(port) != parts[1] {
		return "", 0, false
	}
	suffix := ".svc." + clusterDomain
	if !strings.HasSuffix(parts[3], suffix) {
		return "", 0, false
	}
	host := strings.Split(strings.TrimSuffix(parts[3], suffix), ".")
	if len(host) != 2 {
		return "", 0, false
	}
	service = host[1] + "/" + host[0]
	if ValidateService(service, port) != nil {
		return "", 0, false
	}
	return service, port, true
}
