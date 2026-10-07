package main

import "testing"

func TestIsLocalWorker(t *testing.T) {
	tests := []struct {
		name          string
		localHostname string
		wHostname     string
		wPort         int
		workerPort    int
		want          bool
	}{
		{
			name:          "hostname match exact",
			localHostname: "Davids-MBP",
			wHostname:     "Davids-MBP",
			wPort:         8081,
			workerPort:    8081,
			want:          true,
		},
		{
			name:          "hostname match case-insensitive",
			localHostname: "Davids-MBP",
			wHostname:     "davids-mbp",
			wPort:         8081,
			workerPort:    9090,
			want:          true,
		},
		{
			name:          "hostname mismatch different host same port",
			localHostname: "Davids-MBP",
			wHostname:     "AB-DTD3",
			wPort:         8081,
			workerPort:    8081,
			want:          false,
		},
		{
			name:          "hostname empty on worker – fallback to port match",
			localHostname: "Davids-MBP",
			wHostname:     "",
			wPort:         8081,
			workerPort:    8081,
			want:          true,
		},
		{
			name:          "hostname empty locally – fallback to port match",
			localHostname: "",
			wHostname:     "Davids-MBP",
			wPort:         8081,
			workerPort:    8081,
			want:          true,
		},
		{
			name:          "both hostnames empty – falls back to port match",
			localHostname: "",
			wHostname:     "",
			wPort:         8081,
			workerPort:    8081,
			want:          true,
		},
		{
			name:          "hostname present but port mismatch",
			localHostname: "Davids-MBP",
			wHostname:     "AB-DTD3",
			wPort:         9000,
			workerPort:    8081,
			want:          false,
		},
		{
			name:          "port fallback mismatch",
			localHostname: "",
			wHostname:     "",
			wPort:         9000,
			workerPort:    8081,
			want:          false,
		},
		{
			name:          "worker port zero",
			localHostname: "Davids-MBP",
			wHostname:     "AB-DTD3",
			wPort:         0,
			workerPort:    8081,
			want:          false,
		},
		{
			name:          "workerPort zero fallback fails",
			localHostname: "",
			wHostname:     "",
			wPort:         8081,
			workerPort:    0,
			want:          false,
		},
		{
			name:          "both hostnames empty, ports differ – no match",
			localHostname: "",
			wHostname:     "",
			wPort:         9000,
			workerPort:    8081,
			want:          false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsLocalWorker(tt.localHostname, tt.wHostname, tt.wPort, tt.workerPort)
			if got != tt.want {
				t.Errorf("IsLocalWorker(%q,%q,%d,%d) = %v, want %v",
					tt.localHostname, tt.wHostname, tt.wPort, tt.workerPort, got, tt.want)
			}
		})
	}
}
