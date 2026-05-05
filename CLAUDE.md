# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Purpose

High-performance fraud detection API for the "Rinha de Backend 2026" competition. The system detects fraudulent transactions via k-nearest-neighbor search over 3 million reference transactions using a K-Means IVF index, operating under strict resource constraints: **1.5 CPUs and 550MB RAM total** (shared across 2 API instances + Nginx).

## Commands

```bash
# Generate the optimized binary dataset from references.json (run before building)
make precompute

# Compile the Go+CGO API binary to bin/api
make build

# Build and run locally (no Docker)
make run

# Full stack with Docker Compose (Nginx on :9999, api01 on :9998, api02 on :9997)
docker compose up --build

# Clean binaries and generated dataset
make clean
```

The build requires `CGO_ENABLED=1` and `CGO_CFLAGS=-O3 -mavx2` (set in Makefile). There are no external Go dependencies — stdlib only.

There are no automated tests; use `test_requests.http` for manual HTTP testing against a running instance.

## Architecture

Two executables + one C engine:

### `cmd/precompute` — Offline Pipeline

Reads `./resources/references.json` (3M records) and writes `./resources/dataset_otimizado.bin`. The binary format is:
1. 1024 cluster centroids (1024 × 16 float32)
2. Bucket offsets (1025 uint32)
3. All vectors reordered by bucket (N × 16 float32)
4. Fraud labels reordered (N × uint8)

The IVF clustering picks initial centroids directly from the dataset (no iterative refinement). Both `precompute` and `api` use the same `vectorize()` logic to produce a 16-dimensional float32 vector — **these must stay in sync**.

### `cmd/api` — HTTP Server (port 9999)

Two routes: `GET /ready` (health check) and `POST /fraud-score`.

The hot path: decode JSON → normalize into 16-dim vector → call `search_top_5()` via CGO → return `{"score": float}`.

`sync.Pool` recycles `FraudPayload` structs to minimize GC pressure. The last 2 of 16 vector dimensions are padding (always 0.0) to align SIMD loads.

### `internal/engine/core.c` — C Motor (SIMD)

The performance-critical layer. On startup (`init_memory`), it `mmap`s the binary dataset into process memory (shared across goroutines, read-only). On each request (`search_top_5`):

1. **Centroid scan**: AVX2 dot products over all 1024 centroids to find the nearest cluster.
2. **Bucket search**: Linear scan of only that cluster's vectors; maintains top-5 nearest neighbors via insertion sort.
3. **Score**: `fraud_score = (fraudulent neighbors in top-5) / 5.0`.

AVX2 processes 16 floats (two 256-bit registers) per loop iteration. The dataset is shared read-only via `mmap(MAP_SHARED)`, so both `api01` and `api02` containers map the same physical pages.

### Infrastructure

- **Nginx** (`nginx.conf`): `upstream rinha_api` with keepalive 500 connections to `api01:9999` and `api02:9999`. Access logging is disabled. epoll event model.
- **Docker Compose**: api01 (0.6 CPU / 250MB), api02 (0.6 CPU / 250MB), nginx (0.3 CPU / 50MB).
- **Multi-stage Dockerfile**: Builder stage runs `make precompute && make build`; runtime stage is `debian:bookworm-slim` with only `bin/api` and `resources/dataset_otimizado.bin`.

## Key Constraints and TODOs

- The `resources/` directory is git-ignored. `references.json` must be supplied externally before running `make precompute`.
- MCC risk is hardcoded to `0.5` in the vectorizer — there is a TODO to load from `mcc_risk.json`.
- The platform requires Linux/WSL2 (POSIX `mmap`) and a CPU with AVX2 support.
- Paths for the dataset (`./resources/dataset_otimizado.bin`) and API listen address (`:9999`) are hardcoded.
