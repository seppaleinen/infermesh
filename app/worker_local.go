package main

import "strings"

// IsLocalWorker reports whether the worker described by hostname/port is the
// worker this desktop app supervises (the one that should get the "You" badge).
//
// Hostname match is primary: when both the app and the worker report a
// hostname, an case-insensitive comparison identifies the local worker even
// when a remote host happens to run its worker on the same port (issue #90).
//
// Port match is the fallback: it still works for the supervised-worker case
// when hostname detection fails on either side (e.g. an older worker that
// does not advertise a hostname, or a host whose os.Hostname() is unavailable).
func IsLocalWorker(localHostname, wHostname string, wPort, workerPort int) bool {
	if localHostname != "" && wHostname != "" {
		return strings.EqualFold(wHostname, localHostname)
	}
	if workerPort != 0 && wPort != 0 {
		return wPort == workerPort
	}
	return false
}
