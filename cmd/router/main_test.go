package main

import (
	"testing"
)

// TestParseRouterFlags verifies the router flag set parses correctly, including
// the --max-in-flight and --max-connections caps introduced for issue #54.
func TestParseRouterFlags(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    RouterFlags
		wantErr bool
	}{
		{
			name: "defaults",
			args: []string{},
			want: RouterFlags{
				DevMode:             false,
				ProdMode:            false,
				MTLSCert:            "",
				MTLSKey:             "",
				CertDir:             "",
				APIKey:              "",
				Addr:                ":8080",
				RelayURL:            "",
				MaxInFlight:         0,
				MaxConnections:      0,
				ScorerQuantMatch:    0,
				ScorerVRAMFree:      0,
				ScorerGPUUtil:       0,
				ScorerQueueDepth:    0,
				ScorerLatency:       0,
				ScorerMaxQueueDepth: 0,
			},
		},
		{
			name: "dev-mode with caps",
			args: []string{"--dev-mode", "--max-in-flight", "8", "--max-connections", "512"},
			want: RouterFlags{
				DevMode:             true,
				Addr:                ":8080",
				MaxInFlight:         8,
				MaxConnections:      512,
				ScorerQuantMatch:    0,
				ScorerVRAMFree:      0,
				ScorerGPUUtil:       0,
				ScorerQueueDepth:    0,
				ScorerLatency:       0,
				ScorerMaxQueueDepth: 0,
			},
		},
		{
			name: "dev-mode with scorer weights",
			args: []string{"--dev-mode", "--scorer-quant-match", "0.5", "--scorer-vram-free", "0.3", "--scorer-gpu-util", "0.1", "--scorer-queue-depth", "0.05", "--scorer-latency", "0.05", "--scorer-max-queue-depth", "20"},
			want: RouterFlags{
				DevMode:             true,
				Addr:                ":8080",
				ScorerQuantMatch:    0.5,
				ScorerVRAMFree:      0.3,
				ScorerGPUUtil:       0.1,
				ScorerQueueDepth:    0.05,
				ScorerLatency:       0.05,
				ScorerMaxQueueDepth: 20,
			},
		},
		{
			name:    "bad int flag",
			args:    []string{"--max-in-flight", "not-a-number"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := parseRouterFlags(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if f.DevMode != tt.want.DevMode {
				t.Errorf("DevMode: got %v, want %v", f.DevMode, tt.want.DevMode)
			}
			if f.Addr != tt.want.Addr {
				t.Errorf("Addr: got %q, want %q", f.Addr, tt.want.Addr)
			}
			if f.MaxInFlight != tt.want.MaxInFlight {
				t.Errorf("MaxInFlight: got %d, want %d", f.MaxInFlight, tt.want.MaxInFlight)
			}
			if f.MaxConnections != tt.want.MaxConnections {
				t.Errorf("MaxConnections: got %d, want %d", f.MaxConnections, tt.want.MaxConnections)
			}
		})
	}
}