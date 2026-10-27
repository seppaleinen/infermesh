package worker

import (
	"net"
	"os"
	"strings"
)

// LocalIP returns the first non-loopback IPv4 address of the host, or
// 127.0.0.1 when none can be determined.
func LocalIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "127.0.0.1"
	}
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				return ipnet.IP.String()
			}
		}
	}
	return "127.0.0.1"
}

// Hostname returns the local hostname, falling back to "infermesh-worker"
// when it cannot be determined.
func Hostname() string {
	name, err := os.Hostname()
	if err != nil {
		return "infermesh-worker"
	}
	return name
}

// splitModelList parses a comma-separated model list into a trimmed, non-empty
// slice. Nil input and empty/whitespace-only entries are filtered out so the
// JSON encoder omits the field when nothing was declared.
func splitModelList(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
