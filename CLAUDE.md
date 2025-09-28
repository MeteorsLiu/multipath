# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

This is a TUN-based multipath bandwidth aggregation system written in Go. It allows combining multiple network paths (TCP or UDP) to aggregate bandwidth and provide failover capabilities for network connections.

## Architecture

### Core Components

**Main Entry Points:**
- `main.go`: Entry point that parses config and starts either client or server mode
- `config.go`: JSON configuration parsing with client/server/TUN settings
- `client.go`: Client-side implementation that dials to multiple remote paths
- `server.go`: Server-side implementation that listens for incoming connections

**Key Internal Packages:**

**Scheduler (`internal/scheduler/cfs/`):**
- Implements a Completely Fair Scheduler (CFS) algorithm for path selection
- Uses a min-heap to select paths with lowest virtual sent bytes
- `cfs.go`: Main scheduler implementation with path heap management
- `path.go`: CFS path wrapper that tracks virtual sent bytes and connection states

**Connection Management (`internal/conn/`):**
- `conn.go`: Defines interfaces for batch readers/writers and connection abstractions
- `udpmux/`: UDP multiplexing with connection tracking, probing, and fragmentation
- `tcp/`: TCP connection handling
- Protocol abstraction with packet headers and IP parsing

**Network Probing (`internal/prober/`):**
- `prober.go`: Implements RTT-based connection health monitoring with Holt smoothing
- `manager.go`: Manages multiple probers for different connections
- State machine: Initializing → Normal → Unstable → Lost with automatic recovery

**TUN Interface (`internal/tun/`):**
- `tun.go`: TUN interface handler with read/write loops
- Platform-specific implementations for Linux/Darwin
- Integrates with scheduler for outbound packet routing

**Path Management (`internal/path/`):**
- `path.go`: Connection path abstraction
- `manager.go`: Manages active paths with event callbacks for scheduler integration

**Memory Management (`internal/mempool/`):**
- Buffer pooling system to reduce GC pressure during high-throughput operations

## Development Commands

### Building
```bash
go build .
```

### Testing
```bash
# Run all tests
go test ./...

# Run specific package tests
go test ./internal/conn/udpmux -v

# Run single test
go test ./internal/conn/udpmux -v -run TestReceiver

# Run tests with timeout (important for network tests)
go test ./internal/conn/udpmux -v -timeout 30s
```

### Running
```bash
# Server mode
./multipath -config server_config.json

# Client mode
./multipath -config client_config.json
```

## Configuration

Uses JSON configuration files with these key sections:
- `client.remotePaths`: Array of remote endpoints with weights
- `server.listen`: Server listen address
- `tun`: TUN interface configuration (name, local/remote addresses)
- `tcp`: Boolean flag to use TCP instead of UDP
- `isServer`: Boolean to determine server vs client mode

## Key Design Patterns

**Event-Driven Architecture:**
- Path manager uses callbacks to notify scheduler of path changes
- Prober uses state machine with event callbacks for connection health

**Scheduler Integration:**
- Main application creates both connection path manager and scheduler path manager
- Event callbacks bridge the two systems when connections are added/removed

**Memory Efficiency:**
- Extensive use of buffer pooling (`mempool`) to reduce allocations
- Batch operations for network I/O where possible

**Platform Abstraction:**
- TUN interface has OS-specific implementations
- Build tags separate Linux/Darwin specific code

## Testing Architecture

Tests are organized by package with mock implementations:
- `mock_test.go` files provide isolated testing environments
- Network tests use localhost connections with timeouts
- Prober tests verify state transitions and RTT estimation
- Packet fragmentation and reassembly testing in UDP mux

## Connection Flow

1. **Client**: Creates TUN → Dials to configured remote paths → Registers with path manager → Scheduler assigns paths
2. **Server**: Creates TUN → Listens on configured address → Accepts connections → Registers with path manager
3. **Data Flow**: TUN read → Scheduler selects best path → Connection write → Network → Remote end → TUN write

## Critical Implementation Details

- UDP connections use custom multiplexing with heartbeat probing
- Scheduler uses virtual time to ensure fair bandwidth allocation
- Prober implements sophisticated RTT prediction with Holt smoothing
- Memory buffers include header space reservation for protocol encapsulation
- Path weights affect scheduler priority but actual selection uses virtual sent bytes