# Rinha de Backend 2026 — Motor de Detecção de Fraudes

API de detecção de fraudes em tempo real construída para a **Rinha de Backend 2026**. O sistema classifica transações via busca pelos k-vizinhos mais próximos em um dataset de referência com 3 milhões de vetores, operando sob restrições severas de hardware: **1 CPU e 350 MB de RAM** compartilhados em toda a stack.

**Resultado:** 99,98% de precisão, p99 de 1,95ms, score final de **5.319/6.000**.

---

## Arquitetura

O pipeline é dividido em duas etapas: uma fase de pré-processamento offline e uma fase de serviço em tempo real.

```
references.json ──► [precompute Go] ──► dataset_otimizado.bin ──► [mmap] ──► Motor C AVX2
                                                                                   ▲
Requisição HTTP ──► fasthttp ──► go-json ──► vetorizar (14 dims) ────────────────┘
```

### Offline — `cmd/precompute`

Lê o `references.json` (3 milhões de vetores float[14] + label `fraud`/`legit`), clusteriza os vetores em 2.048 buckets usando **K-Means++ com 15 iterações**, e serializa o resultado em formato binário compacto com vetores quantizados em **float16**.

- **K-Means++**: cada centróide inicial é escolhido com probabilidade proporcional à distância² ao centróide mais próximo já colocado, garantindo dispersão máxima e convergência para um mínimo melhor
- Atribuição paralela usando todos os CPUs disponíveis (`runtime.NumCPU()` goroutines)
- Vetores armazenados em float16 (IEEE 754 half precision): dataset de ~190 MB reduzido para ~95 MB, aumentando a taxa de acerto no cache L3
- Escrita via `bufio.Writer` com buffer de 8 MB (minimiza syscalls)

O formato binário gerado é autocontido:

| Seção | Tamanho |
|---|---|
| 2.048 centróides | 2048 × 16 × 4 bytes (float32) |
| Offsets dos buckets | 2049 × 4 bytes (uint32) |
| Vetores (reordenados por bucket) | N × 16 × 2 bytes (float16) |
| Labels (reordenados por bucket) | N × 1 byte (uint8) |

### Runtime — `cmd/api`

Recebe `POST /fraud-score`, normaliza os campos da transação em um vetor float32 de 14 dimensões (preenchido até 16 para alinhamento SIMD) e delega ao motor C via pacote `internal/engine`.

```
Requisição → fasthttp → go-json decode (sync.Pool) → vetorizar → engine.GetFraudScore → go-json encode
```

- `fasthttp`: zero alocações no hot path, parsing de headers otimizado
- `go-json` (goccy): 2-3x mais rápido que `encoding/json`
- `sync.Pool` recicla `FraudPayload`, eliminando pressão de GC no caminho crítico
- `GOGC=off` + `GOMEMLIMIT=115MiB`: coleta de lixo só por pressão de memória, sem spikes de latência no p99

### Motor C — `internal/engine`

Exposto ao Go por uma fronteira CGO limpa (`fraud.go` → `core.h`). Na inicialização, `mmap(MAP_SHARED)` projeta o dataset binário direto na memória do processo — sem leituras de arquivo em tempo de requisição, e ambas as instâncias do container compartilham as mesmas páginas físicas.

Por requisição (`search_top_5`):

1. **Fase 1 — scan dos centróides:** distância euclidiana ao quadrado via AVX2 (`_mm256_loadu_ps`, dois registradores de 256 bits cobrindo os 16 floats) contra todos os 2.048 centróides. Insertion sort mantém os 10 mais próximos (`NPROBE = 10`)
2. **Fase 2 — scan dos buckets:** varredura linear dos vetores nos 10 clusters selecionados. `__builtin_prefetch` com distância de 8 vetores esconde a latência de acesso à RAM. Vetores são lidos em float16 e convertidos para float32 via instrução F16C (`_mm256_cvtph_ps`). Top-5 global mantido via insertion sort
3. **Pontuação:** `fraud_score = (vizinhos fraudulentos no top-5) / 5.0`

AVX2 processa 16 floats por iteração de loop (dois registradores de 256 bits), compilado com `-O3 -march=haswell`.

### Infraestrutura

| Serviço | CPU | RAM | Função |
|---|---|---|---|
| api01 | 0,4 | 140 MB | Instância primária |
| api02 | 0,4 | 140 MB | Instância secundária |
| nginx | 0,2 | 70 MB | Load balancer |

O Nginx é configurado com `epoll`, pool de keepalive de 500 conexões para os upstreams e logging completamente desativado. Comunicação API↔Nginx via **unix socket** — elimina o stack TCP interno e reduz ~100-200µs de latência por requisição em Linux nativo.

---

## Pré-requisitos

Linux ou WSL 2 (POSIX `mmap`), CPU com suporte a AVX2 e F16C, Go 1.22+, GCC, Make, Docker.

```bash
# Debian/Ubuntu
sudo apt install golang make gcc
```

---

## Execução

### Via Docker (recomendado)

Coloque o `references.json` em `./resources/` e execute:

```bash
docker compose up --build
```

O Dockerfile executa `make precompute` e `make build` dentro do estágio de build. O precompute roda K-Means++ sobre os 3 milhões de registros — espere ~4 minutos de build.

```bash
# Health check
curl -i http://localhost:9999/ready

# Fraud score
curl -s -X POST http://localhost:9999/fraud-score \
  -H "Content-Type: application/json" \
  -d @payload.json
```

### Localmente

```bash
# 1. Gera o dataset binário (requer ./resources/references.json)
make precompute

# 2. Compila e sobe a API
make run
```

---

## Resposta

```json
{
  "approved": false,
  "fraud_score": 0.8
}
```

`fraud_score` está no intervalo [0,0 — 1,0]. Transações com `fraud_score >= 0,6` são rejeitadas (`approved: false`).

---

## Decisões de Design

- **K-Means++ com 2.048 clusters:** a inicialização por distância² garante dispersão máxima dos centróides iniciais. 2.048 clusters criam fronteiras mais finas que 1.024 — buckets médios de ~1.465 vetores vs ~2.930. O sweet spot foi encontrado empiricamente: 2.048/NPROBE=10 escaneia ~14.650 vetores por requisição com fronteiras mais precisas do que 1.024/NPROBE=5 com o mesmo volume de scan
- **NPROBE = 10:** busca nos 10 clusters mais próximos por query. A penalidade de detecção é logarítmica (retorno decrescente por erro fixado) enquanto o custo de latência é linear — acima de NPROBE=10, o custo supera o ganho
- **float16 nos vetores:** reduz o dataset à metade (~95 MB), aumentando a eficiência de cache L3. A conversão F16C via `_mm256_cvtph_ps` é uma instrução AVX — sem custo computacional relevante
- **GOGC=off + GOMEMLIMIT=115MiB:** GC periódico desativado; coleta só por pressão de memória, eliminando spikes de latência no p99
- **Unix socket nginx → API:** elimina o stack TCP interno, reduzindo ~100-200µs por requisição em Linux nativo
- **mmap MAP_SHARED:** ambas as instâncias mapeiam o mesmo arquivo físico sem duplicação de RAM para o dataset de ~95 MB

---

## Jornada e experimentos

A história completa — bugs encontrados, experimentos com NPROBE e número de clusters, e a análise de tradeoff que levou ao resultado final — está documentada em [JORNADA.md](./JORNADA.md).
