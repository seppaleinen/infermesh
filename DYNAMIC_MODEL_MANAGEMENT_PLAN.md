# Dynamic Model Management Implementation Plan

## Overview
This plan implements comprehensive model lifecycle management for InferMesh workers, enabling dynamic model loading/unloading, health monitoring with TTL-based auto-management, and failover capabilities.

## Key Features

### 1. Enhanced Backend Model Management
- **Actual model loading/unloading** for each backend (llama.cpp, Ollama, LM Studio, vLLM)
- **Model state tracking** (Loaded/Unloading/Unloaded/HealthStatus)
- **Backend-specific load paths** and cleanup procedures
- **Error handling** for failed load attempts with circuit breaker integration

### 2. Model Health & TTL Management
- **Health monitoring** with configurable TTL (Time To Live)
- **Auto-unload** of inactive models after TTL expiration
- **Periodic health checks** for loaded models
- **Health-based failover** - switch to healthy models if current is unhealthy
- **Resource tracking** for GPU memory and CPU usage

### 3. Dynamic Model Switching
- **Load/Unload endpoints** in worker HTTP API (`/v1/models/load`, `/v1/models/unload`)
- **Model status queries** (`/v1/models/{model}/status`)
- **Auto-switch endpoints** for immediate model changes
- **Load balancing** between models based on availability and health

### 4. Integration with Circuit Breakers
- **Health-based circuit opening** when models become unhealthy
- **Circuit recovery** when models become healthy again
- **Failover routing** to alternative models
- **Health monitoring** threads for continuous state updates

### 5. Enhanced Metrics & Monitoring
- **Detailed model metrics** (loads, unloads, latency, VRAM usage)
- **Health check metrics** with success/failure tracking
- **Resource usage monitoring** per model
- **Circuit breaker metrics** for failover decisions
- **Performance dashboards** for operational visibility

### 6. Enhanced BackendAdapter
- **Model switching logic** during request processing
- **Health-aware routing** - prefer healthy models
- **Automatic fallback** to healthy alternatives
- **Graceful degradation** when all models fail

### 7. Testing & Verification
- **Unit tests** for all model lifecycle operations
- **Integration tests** for end-to-end workflows
- **Health monitoring tests** with simulated failures
- **Fallback mechanism tests**
- **Regression tests** to ensure 240+ existing tests still pass

## Implementation Steps

### Phase 1: Backend Model Management (Week 1-2)

#### Core Backend Interface
- Enhance `LoadModel` method to actually load models
- Implement `UnloadModel` with proper cleanup
- Add `ModelState` and `HealthStatus` fields to ModelInfo
- Add `HealthCheck()` method to Backend interface

#### Backend Implementations
- **Llama.cpp**: Use llama.cpp API to load/unload GGUF models
- **Ollama**: Implement `/api/pull` and `/api/delete` endpoints
- **LM Studio**: Use local file system and API for model management
- **vLLM**: Use REST API for model lifecycle operations

#### Health Monitoring System
- Add `ModelHealthTracker` with TTL support
- Implement `LoadHealthTracker` and `UnloadHealthTracker`
- Add background cleanup routine for expired models
- Integrate with circuit breaker for health monitoring

### Phase 2: Worker HTTP API (Week 3)

#### New Endpoints
```
POST /v1/models/load    - Load a specific model
POST /v1/models/unload  - Unload a model
GET  /v1/models/{name}/status - Get model health status
GET  /v1/models/health    - Get overall health summary
POST /v1/models/switch   - Switch active model for backend adapter
```

#### Request/Response Models
- `LoadModelRequest`: model name, parameters
- `LoadModelResponse`: success, model info
- `ModelHealth`: last_check, status, metrics
- `ModelStatusResponse`: overall health

#### Implementation Details
- **Atomic operations** with proper locking
- **Health validation** before loading models
- **Resource constraints** for model loading
- **Error handling** with OpenAI-compatible error responses

### Phase 3: Dynamic Switching & Failover (Week 4)

#### BackendAdapter Integration
- Add `SwitchModel()` method for runtime model switching
- Modify request handlers to use active model from backend adapter
- Implement **health-aware routing** - always prefer healthy models
- Add **automatic fallback** when current model becomes unhealthy

#### Circuit Breaker Integration
- **Health-based circuit opening** when models become unhealthy
- **Model health polling** for continuous monitoring
- **Graceful failover** to healthy alternative models
- **Recovery detection** when unhealthy models become healthy

#### Metrics & Monitoring
- **Enhanced Metrics** struct with model-specific metrics
- **Load/Unload counters** for each model
- **Health check tracking** with timestamps and results
- **Resource usage metrics** for each loaded model

### Phase 4: Testing & Verification (Week 5)

#### Unit Tests
- **Backend tests**: Mock backend implementations with controlled load/unload behavior
- **Health tracking tests**: TTL expiration, health checks, auto-cleanup
- **API endpoint tests**: Load/unload endpoints with various scenarios
- **Failover tests**: Unhealthy model detection and failover

#### Integration Tests
- **Full model lifecycle**: Load -> Use -> Unload -> Load again
- **Health monitoring integration**: Real backends with simulated failures
- **Failover scenarios**: Backend health degradation and recovery
- **Resource constraints**: Model loading limits and VRAM tracking

#### Regression Tests
- **Existing functionality**: Ensure 240+ existing tests still pass
- **Backward compatibility**: Existing API calls still work
- **Performance**: No significant performance degradation

## Technical Implementation Details

### Model State Management
```go
type ModelState int

const (
    ModelUnloaded ModelState = iota // Model not loaded
    ModelLoading                   // In process of loading
    ModelLoaded                    // Successfully loaded
    ModelUnloading                 // In process of unloading
    ModelError                     // Failed to load/unload
)
```

### Health Status
```go
type HealthStatus int

const (
    Healthy HealthStatus = iota
    Degraded
    Unhealthy
    Unknown
)
```

### ModelInfo Extensions
```go
type ModelInfo struct {
    // Existing fields...
    ModelName      string
    State          ModelState
    Health         HealthStatus
    LastChecked    time.Time
    LoadTime       time.Time
    UnloadTime     time.Time
    TTL            time.Duration
    HealthTracker  *ModelHealthTracker
    CircuitBreaker *CircuitBreaker
}
```

### ModelHealthTracker
```go
type ModelHealthTracker struct {
    LastHealthCheck time.Time
    HealthStatus    HealthStatus
    ConsecutiveFailures int
    Metrics          ModelMetrics
}

type ModelMetrics struct {
    LoadCount     int
    UnloadCount   int
    TotalLatency  time.Duration
    VramUsageMB   int
    GpuUsagePct   int
    RequestCount   int
    ErrorCount     int
}
```

## Testing Strategy

### Unit Tests
- **Backend-specific load/unload operations**
- **Health tracking with TTL**
- **Circuit breaker integration**
- **Model state transitions**

### Integration Tests
- **Full model lifecycle workflows**
- **Health monitoring and failover**
- **Worker API endpoints**
- **Resource constraint enforcement**

### Performance Tests
- **Model loading times**
- **Memory usage patterns**
- **Health check overhead**
- **Failover response times**

## Rollout Plan

### Phase 1 (Week 1-2): Backend Enhancements
- Implement actual LoadModel/UnloadModel methods
- Add health tracking infrastructure
- Unit tests for backend operations

### Phase 2 (Week 3): Worker API
- Add HTTP endpoints for model management
- Implement health status endpoints
- Integration tests for API

### Phase 3 (Week 4): Dynamic Switching
- Implement runtime model switching
- Add circuit breaker integration
- Add failover logic

### Phase 4 (Week 5): Testing & Verification
- Comprehensive testing
- Load testing
- Documentation and examples

## Risk Mitigation

### Technical Risks
- **Backend compatibility**: Test against all supported backends
- **Resource constraints**: Implement proper limits and monitoring
- **Performance impact**: Benchmark before/after

### Operational Risks
- **Model instability**: Robust health monitoring and failover
- **Resource leaks**: Proper cleanup and resource tracking
- **Configuration complexity**: Clear documentation and examples

### Testing Risks
- **Test coverage**: Comprehensive unit and integration tests
- **Test environment**: Realistic test scenarios
- **Regression**: Ensure existing functionality preserved

## Success Metrics

### Technical
- **All tests pass**: 240+ existing + new tests
- **Model load times**: < 30 seconds for typical models
- **Health check frequency**: Configurable, default 30s
- **Failover response**: < 5 seconds

### Operational
- **Model availability**: 99.9% uptime
- **Resource efficiency**: Proper cleanup and management
- **Monitoring visibility**: Comprehensive metrics and alerts
- **Documentation**: Complete API documentation

### Performance
- **Throughput**: No significant degradation
- **Latency**: Acceptable for typical workloads
- **Resource usage**: Efficient and predictable

## Conclusion

This plan implements comprehensive dynamic model management with health monitoring, failover, and resource management capabilities. The implementation is phased to ensure reliable rollout and thorough testing. All changes maintain backward compatibility while adding powerful new features for production deployments.