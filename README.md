# Rinha de Backend 2026 — Motor de Detecção de Fraudes

API de detecção de fraudes em tempo real construída para a **Rinha de Backend 2026**. O sistema classifica transações via busca pelos k-vizinhos mais próximos em um dataset de referência com 3 milhões de vetores, operando sob restrições severas de hardware: **1,5 CPUs e 550 MB de RAM** compartilhados em toda a stack.

---

## Arquitetura

O pipeline é dividido em duas etapas: uma fase de pré-processamento offline e uma fase de serviço em tempo real.

```
references.json ──► [precompute] ──► dataset_otimizado.bin ──► [mmap] ──► Motor C (AVX2)
                                                                               ▲
Requisição HTTP ──► API Go ──► vetorizar (14 dims) ────────────────────────────┘
```

### Offline — `cmd/precompute`

Lê o `references.json` (vetores float[14] já pré-computados + label "legit"/"fraud"), clusteriza os 3M vetores em 1.024 buckets usando **K-Means com refinamento iterativo completo**, e serializa o resultado em um formato binário compacto.

- Inicialização dos centróides por amostragem aleatória (elimina viés de posição no arquivo)
- Atribuição paralela usando todos os CPUs disponíveis (`runtime.NumCPU()` goroutines)
- Atualização de centróides recalcula a média real de cada cluster; clusters vazios são reinicializados com vetores sorteados
- Escrita via `bufio.Writer` com buffer de 8 MB (minimiza syscalls)

O formato binário gerado é autocontido:

| Seção | Tamanho |
|---|---|
| 1.024 centróides | 1024 × 16 × 4 bytes |
| Offsets dos buckets | 1025 × 4 bytes |
| Vetores (reordenados por bucket) | N × 16 × 4 bytes |
| Labels (reordenados por bucket) | N × 1 byte |

### Runtime — `cmd/api`

Recebe `POST /fraud-score`, normaliza os campos da transação em um vetor float32 de 14 dimensões (preenchido até 16 para alinhamento SIMD) e delega ao motor C via pacote `internal/engine`.

```
Requisição → fasthttp → go-json decode (sync.Pool) → vetorizar → engine.GetFraudScore → go-json encode
```

### Motor C — `internal/engine`

Exposto ao Go por uma fronteira CGO limpa (`fraud.go` → `core.h`). Na inicialização, `mmap(MAP_SHARED)` projeta o dataset binário direto na memória do processo — sem leituras de arquivo em tempo de requisição, e ambas as instâncias do container compartilham as mesmas páginas físicas.

Por requisição (`search_top_5`):

1. **Varredura de centróides** — produtos internos AVX2 nos 1.024 centróides; insertion sort mantém os 3 mais próximos (`nprobe = 3`)
2. **Busca nos buckets** — varredura linear nos 3 clusters selecionados; top-5 global mantido via insertion sort
3. **Pontuação** — `fraud_score = (vizinhos fraudulentos no top-5) / 5,0`

AVX2 processa 16 floats por iteração de loop (dois registradores de 256 bits via `_mm256_loadu_ps`), compilado com `-O3 -mavx2`.

### Infraestrutura

| Serviço | CPU | RAM | Função |
|---|---|---|---|
| api01 | 0,6 | 250 MB | Instância primária |
| api02 | 0,6 | 250 MB | Instância secundária |
| nginx | 0,3 | 50 MB | Load balancer |

O Nginx é configurado com `epoll`, pool de keepalive de 500 conexões para os upstreams e logging completamente desativado.

---

## Pré-requisitos

Linux ou WSL 2 (POSIX `mmap`), CPU com suporte a AVX2, Go 1.22+, GCC, Make, Docker.

```bash
# Debian/Ubuntu
sudo apt install golang make gcc

# Fedora/RHEL
sudo dnf install golang make gcc
```

---

## Execução

### Via Docker (recomendado)

Coloque o `references.json` em `./resources/` e execute:

```bash
docker compose up --build
```

O Dockerfile executa `make precompute` e `make build` dentro do estágio de build, dispensando qualquer configuração local além do arquivo de dados.

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

- **nprobe = 3**: a busca varre os 3 clusters mais próximos e mantém um top-5 global, compensando imperfeições do clustering e melhorando o recall em vetores próximos de fronteiras de cluster
- **GOGC=off + GOMEMLIMIT=210MiB**: GC periódico desativado; coleta só ocorre por pressão de memória, eliminando spikes de latência no p99
- **Unix socket nginx → API**: elimina o stack TCP interno, reduzindo ~100–200µs por requisição em Linux nativo
- **K-Means sem K-Means++**: inicialização aleatória com 5 iterações de refinamento completo — suficiente para distribuição uniforme dos clusters sem custo proibitivo no build
