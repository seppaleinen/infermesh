package protocol

import "time"

// WorkerStatus indicates the availability state of a worker.
type WorkerStatus string

const (
	StatusAvailable   WorkerStatus = "available"
	StatusBusy        WorkerStatus = "busy"
	StatusUnassigned  WorkerStatus = "unassigned"
	StatusUnavailable WorkerStatus = "unavailable"
)

// GPUInfo describes the GPU hardware of a worker.
type GPUInfo struct {
	Vendor      string `json:"vendor"`         // "nvidia", "amd", "apple"
	Model       string `json:"model"`           // "RTX 4090", "M2 Ultra"
	ComputeCore int    `json:"compute_cores"`  // CUDA cores / GPU cores
	TotalVRAM   int64  `json:"total_vram_mb"`  // total VRAM in megabytes
	FreeVRAM    int64  `json:"free_vram_mb"`   // currently free VRAM
}

// MemoryInfo describes system memory available to the worker.
type MemoryInfo struct {
	TotalMB int64 `json:"total_mb"`
	FreeMB  int64 `json:"free_mb"`
}

// ModelInfo describes a single model loaded or known to the worker.
type ModelInfo struct {
	Name        string   `json:"name"`            // e.g. "llama-3-8b"
	Size        int64    `json:"size_bytes"`       // model file size
	Quantization string  `json:"quantization"`    // "Q4_K_M", "FP16", etc.
	MaxTokens   int      `json:"max_tokens"`       // context window
	Backend     string   `json:"backend"`          // "llama-cpp", "ollama", etc.
	Loaded      bool     `json:"loaded"`           // whether model is in VRAM
}

// EngineType represents supported backend engines.
type EngineType string

const (
	EngineLLMCPP   EngineType = "llama-cpp"
	EngineOllama   EngineType = "ollama"
	EngineLMStudio EngineType = "lmstudio"
	EngineVLLM     EngineType = "vllm"
)

// Transport identifiers for WorkerInfo.Transport.
// Legacy/mDNS workers leave Transport empty, which the router treats as
// TransportHTTP (dial-back). WebSocket workers set TransportWS and are
// reached through the outbound connection held by the router's WS hub.
const (
	TransportHTTP = "http"
	TransportWS   = "ws"
)

// Capabilities describes what a worker can do.
type Capabilities struct {
	GPU     GPUInfo     `json:"gpu"`
	Models  []ModelInfo `json:"models"`
	Engines []string   `json:"engines"`  // supported backends ["llama-cpp","vllm",...]
	VRAM    MemoryInfo  `json:"vram"`
	System  MemoryInfo  `json:"system"`
}

// WorkerInfo is the canonical payload carried in mDNS TXT records.
// Both worker (announcing) and router (receiving) use this type.
type WorkerInfo struct {
	ID           string       `json:"id"`                  // unique worker identifier
	Hostname     string       `json:"hostname"`            // machine hostname
	IP           string       `json:"ip"`                  // resolved IPv4 address
	Port         int          `json:"port"`                // HTTP API port
	Capabilities Capabilities `json:"capabilities"`        // GPU, models, etc.
	Status       WorkerStatus `json:"status"`              // availability
	Version      string       `json:"version"`             // InferMesh protocol version
	Transport    string       `json:"transport,omitempty"` // "ws" for outbound WebSocket workers; empty/"http" for dial-back
	APIKey       string       `json:"api_key,omitempty"`   // optional API key for authentication
	LastSeen     time.Time    `json:"-"`                   // set by router; not serialized
}

// DiscoveryEventType categorizes a discovery event.
type DiscoveryEventType string

const (
	EventAdded   DiscoveryEventType = "added"     // new worker discovered
	EventUpdated DiscoveryEventType = "updated"   // existing worker refreshed
	EventRemoved DiscoveryEventType = "removed"   // worker TTL expired or explicitly gone
	EventExpired DiscoveryEventType = "expired"   // worker marked unavailable by registry
)

// DiscoveryInfo is the lightweight payload carried in mDNS TXT records.
// It contains only the metadata needed for the router to locate and
// contact a worker; full Capabilities are fetched over HTTP after discovery.
type DiscoveryInfo struct {
	ID       string       `json:"id"`
	Hostname string       `json:"hostname"`
	IP       string       `json:"ip"`
	Port     int          `json:"port"`
	Version  string       `json:"version"`
	Status   WorkerStatus `json:"status,omitempty"`
}

// DiscoveryEvent is emitted by a Discovery implementation and consumed by the Registry.
type DiscoveryEvent struct {
	Type   DiscoveryEventType `json:"type"`
	Worker WorkerInfo         `json:"worker"`
	Time   time.Time         `json:"time"`
}