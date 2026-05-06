# Jornada: Rinha de Backend 2026 — Motor de Detecção de Fraudes

Este documento narra as decisões técnicas, experimentos e aprendizados que levaram ao resultado final de **5319 pontos** e **99,98% de precisão** na detecção de fraudes.

---

## O desafio

Construir uma API de detecção de fraude que, para cada transação recebida, transforma o payload em um vetor de 14 dimensões e busca os 5 vizinhos mais próximos num dataset de 3 milhões de referências. A decisão é simples: `fraud_score = fraudes_entre_os_5 / 5`, e `approved = score < 0.6`.

O twist: a stack inteira — dois servidores de API + nginx — não pode usar mais de **1 CPU e 350 MB de RAM**. Latência, precisão e eficiência de memória competem diretamente. A solução, para adição de dificuldade, não poderia usar quaisquer tipo de banco vetorial e deveria combinar elegância e performance.

---

## Arquitetura base

A primeira decisão foi a mais importante: não usar biblioteca de busca vetorial pronta. O dataset é fixo, as dimensões são conhecidas (14, com 2 de padding para alinhamento SIMD), e as restrições de recurso tornam qualquer overhead inaceitável.

A solução: um motor C com AVX2 chamado via CGO, sobre um índice K-Means IVF pré-computado em Go.

```
references.json ──► [precompute Go] ──► dataset_otimizado.bin ──► [mmap] ──► Motor C AVX2
                                                                                   ▲
Requisição HTTP ──► fasthttp ──► go-json ──► vetorizar (14 dims) ────────────────┘
```

**Escolhas do servidor HTTP:**
- `fasthttp` em vez de `net/http`: zero alocações no hot path, parsing de headers otimizado
- `go-json` (goccy): 2-3x mais rápido que `encoding/json` com a mesma interface
- `sync.Pool` reciclando `FraudPayload`: elimina pressão de GC no caminho crítico
- `GOGC=off` + `GOMEMLIMIT=115MiB`: GC periódico desativado, coleta só por pressão de memória — sem spikes de latência no p99

**Unix socket nginx → API:** elimina o stack TCP interno (~100-200µs por requisição em Linux nativo). O Nginx roteia via socket file compartilhado por volume Docker.

---

## O motor C: SIMD e float16

O `search_top_5` em `core.c` opera em duas fases por requisição:

**Fase 1 — scan dos centróides:** distância euclidiana ao quadrado via AVX2 (`_mm256_loadu_ps`, dois registradores de 256 bits cobrindo os 16 floats) contra todos os 2048 centróides. Insertion sort mantém os NPROBE mais próximos.

**Fase 2 — scan dos buckets:** varredura linear dos vetores em cada cluster selecionado. `__builtin_prefetch` com distância 8 vetores à frente esconde a latência de acesso à RAM.

Os vetores no dataset são armazenados em **float16 (IEEE 754 half precision)**, convertidos em runtime para float32 via instrução F16C (`_mm256_cvtph_ps`). Isso reduz o dataset de ~190 MB para ~95 MB — metade do tamanho — permitindo que mais dados caibam no cache L3 da CPU. Ambas as instâncias de API mapeiam o mesmo arquivo via `mmap(MAP_SHARED)`, compartilhando as páginas físicas sem duplicação de RAM.

---

## Os bugs que custaram caro

### Bug 1: cache do Docker mascarando todas as mudanças

Os primeiros testes após correções continuavam retornando 592 FP + 580 FN — exatamente os mesmos números. A hipótese inicial era edge cases na vetorização. A causa real: o test runner cacheava a imagem `:latest` e nunca puxava o código novo.

**Solução:** usar tags com SHA do commit (`mpsiqueira/rinha-2026:f024d1d`) na submission branch. Simples, mas custou vários ciclos de teste para identificar.

### Bug 2: day_of_week com convenção errada

O algoritmo de Sakamoto retorna `domingo=0, segunda=1, ..., sábado=6`. A documentação da Rinha exige `segunda=0, ..., domingo=6`. A diferença de uma divisão por 6 espalhava 0.143 de erro sistemático em todos os vetores — suficiente para deslocar centenas de transações para o cluster errado.

```go
// Sakamoto: dom=0..sáb=6
w := (y + y/4 - y/100 + y/400 + t[m-1] + d) % 7
// Conversão para seg=0..dom=6 (conforme doc):
w = (w + 6) % 7
```

**Impacto:** após corrigir esse bug (e resolver o cache do Docker), os erros caíram de **1172 para 24** numa única submissão. Foi o maior salto de qualidade de toda a jornada.

### Bug 3: sentinel -1 trocado por 0

Quando `last_transaction` é `null`, as dimensões 5 e 6 do vetor devem ser `-1` — um sentinel explícito que agrupa esses casos numa região separada do espaço vetorial. Em algum refactor, tinham virado `0.0`, misturando transações sem histórico com transações de intervalo/distância zero.

---

## A busca pelos últimos erros: experimentação sistemática

Com os bugs corrigidos, restavam ~24 erros consistentes. A causa é estrutural: **erros de recall da ANN**. O verdadeiro vizinho mais próximo cai num cluster que não está entre os NPROBE mais próximos do centróide da query.

Cada experimento foi medido e comparado pelo score final — a fórmula penaliza tanto latência (log₁₀ do p99) quanto erros de detecção (log₁₀ de erros ponderados).

### K-Means++: inicialização inteligente dos centróides

A inicialização aleatória tende a colocar dois centróides iniciais no mesmo aglomerado, criando uma lacuna no espaço. K-Means++ escolhe cada centróide com probabilidade proporcional à distância² ao mais próximo já escolhido:

$$P(x_i) = \frac{D(x_i)^2}{\sum_j D(x_j)^2}$$

Isso maximiza a dispersão inicial e garante convergência para um mínimo melhor. A garantia teórica (Arthur & Vassilvitskii, 2007): inércia inicial $O(\log k)$ vezes o ótimo global.

**Resultado:** FP caiu de 14 para 9, FN manteve em 6. Score subiu de ~5268 para **5282**.

### NPROBE: o parâmetro mais sensível

NPROBE controla quantos buckets são varridos na fase 2. É o principal alavanca de recall vs. latência:

| NPROBE | Vetores escaneados (fase 2) | FP | FN | W | p99 | Score |
|---|---|---|---|---|---|---|
| 5 | ~14.650 | 9 | 6 | 27 | 1.92ms | 5282 |
| 20 | ~58.600 | 8 | 3 | 17 | 3.73ms | 5051 |

NPROBE=20 reduziu erros, mas a latência explodiu. O motivo é matemático: a penalidade de detecção é `−β·log(1+E)` — logarítmica, com retorno decrescente. Corrigir 10 erros ponderados valeu +58 pontos de detecção. Mas o p99 subindo de 1.92ms para 3.73ms custou −288 pontos de latência. **Score líquido: −230 pontos.**

### 2048 clusters: fronteiras mais finas

Dobrar o número de clusters reduz o bucket médio de ~2.930 para ~1.465 vetores. A hipótese: fronteiras mais precisas reduzem casos de borda onde o vizinho verdadeiro fica no cluster errado.

| Clusters | NPROBE | FP | FN | W | p99 | Score |
|---|---|---|---|---|---|---|
| 1024 | 5 | 9 | 6 | 27 | 1.92ms | 5282 |
| 2048 | 5 | 13 | 9 | 40 | 1.68ms | 5291 |

Resultado inesperado: 2048 clusters **piorou** a detecção. Buckets menores têm menos vetores para estabilizar a razão fraude/legítimo — mais ruído por bucket, não menos. Porém a latência caiu para 1.68ms (fase 2 é mais rápida com buckets menores), gerando score marginalmente melhor apesar dos erros adicionais.

### O sweet spot: 2048 clusters + NPROBE=10

A observação-chave: com 2048 clusters, cada NPROBE custa menos latência porque os buckets são menores. Isso abre espaço para aumentar o NPROBE sem sacrificar o p99:

| Config | Vetores na fase 2 | p99 | Score |
|---|---|---|---|
| 1024 clusters, NPROBE=5 | ~14.650 | 1.92ms | 5282 |
| 2048 clusters, NPROBE=10 | ~14.650 | 1.95ms | **5319** |

Mesma quantidade de vetores escaneados, mesma latência — mas sobre fronteiras de cluster mais finas. O resultado foi o melhor de toda a jornada: **FP=10, FN=3, W=19, p99=1.95ms, score=5319**.

---

## Resultado final

| Métrica | Valor |
|---|---|
| Total de transações testadas | 54.100 |
| True Positives (fraudes bloqueadas) | 24.034 |
| True Negatives (legítimas aprovadas) | 30.012 |
| False Positives (legítimas bloqueadas) | 10 |
| False Negatives (fraudes aprovadas) | 3 |
| Precisão | **99,98%** |
| p99 de latência | **1,95ms** |
| Score de detecção | 2.609 / 3.000 |
| Score de latência | 2.709 / 3.000 |
| **Score final** | **5.319 / 6.000** |

---

## Lições aprendidas

**Medir antes de otimizar.** Cada experimento foi submetido e medido com a fórmula real. NPROBE=20 parecia promissor até os números mostrarem que a latência dominava o score.

**A função logarítmica muda o jogo.** Com penalidades em log, cada erro fixado vale menos que o anterior. Isso tem uma implicação contraintuitiva: depois de certo ponto, corrigir mais erros custa mais em latência do que vale em detecção.

**Cache do Docker destrói reprodutibilidade.** Tags com SHA do commit são obrigatórias em ambientes de CI que não controlam o cache do runtime.

**K-Means++ vale sempre.** A inicialização superior garantiu por si só uma redução de 5 FPs sem nenhum custo de latência. É uma melhoria grátis em relação à inicialização aleatória.

**Mais clusters nem sempre significa melhor detecção.** O underfitting de buckets pequenos é real — abaixo de ~1.500 vetores por bucket, a razão fraude/legítimo fica instável. A granularidade de clusters deve ser balanceada contra o tamanho do dataset.

**O verdadeiro gargalo era estrutural.** Os 13 erros que restaram são provavelmente irredutíveis com essa vetorização: transações onde as 14 dimensões não criam separação suficiente no espaço euclidiano entre fraude e legítimo. Não é um bug — é o limite do modelo de features.
