# 🚀 Rinha de Backend 2026 - Motor Vetorial de Alta Performance (Go + CGO + SIMD)

Este repositório contém uma solução de extrema performance desenhada para a **Rinha de Backend 2026** (Detecção de Fraudes via Busca Vetorial).

O desafio exige processar buscas vetoriais em uma base de **3 milhões de transações** operando sob restrições severas de hardware: **1.5 CPUs e 550MB de RAM** no total.

Para sobreviver e dominar esse ambiente, esta arquitetura abandona o processamento tradicional de JSON em tempo de execução e a varredura linear (*brute force*), descendo até o *Bare Metal* com Go e C.

---

## 🧠 A Arquitetura (A "Receita Secreta")

Nossa API foi desenhada com 4 pilares focados em latência na casa dos microssegundos:

1. **AOT Precompute (K-Means IVF):** Antes da API subir, um script em Go lê o `references.json`, aplica a normalização das 14 dimensões oficiais, e clusteriza os 3 milhões de vetores em 1.024 "bairros" usando um Índice de Arquivo Invertido (IVF). O resultado é salvo em um arquivo binário (`.bin`) perfeitamente alinhado.
2. **Zero-Copy Memory (`mmap`):** A API não lê arquivos no boot. O motor em C faz uma chamada direta ao Kernel do Linux via `mmap`, projetando os ~195MB do arquivo binário direto na memória RAM instantaneamente, compartilhando a mesma página de memória entre as instâncias do container.
3. **Hardware Intrinsics (AVX2 SIMD):** A busca da Distância Euclidiana não usa loops comuns em C. Usamos instruções vetoriais `AVX2` de 256-bits (`immintrin.h`) para processar as 16 dimensões do vetor simultaneamente na CPU.
4. **Zero-Allocation Hot Path:** O servidor web em Golang (`net/http`) utiliza `sync.Pool` para reciclar as estruturas de payload do JSON. Durante o teste de estresse do Gatling, o *Garbage Collector* do Go fica com virtualmente zero pressão.

---

## 🛠️ Pré-requisitos

Como utilizamos chamadas nativas POSIX (`mmap`) e otimizações de CPU, o ambiente de desenvolvimento ideal é Linux (ou Windows via WSL 2).

* **Go** (1.22 ou superior)
* **Make**
* **Compilador C** (GCC ou Clang)
* **Podman** (ou Docker)
* **Podman-Compose** (ou Docker Compose)

Para instalar a base de compilação em distribuições baseadas em RedHat/Fedora:
```bash
sudo dnf install golang make gcc clang -y
```

---

## 🏗️ Como Executar

### 1. Ingestão e Pré-processamento
Coloque o arquivo de dados original da Rinha (`references.json`) dentro da pasta `./resources/` na raiz do projeto. Em seguida, gere o banco de dados otimizado:

```bash
make precompute
```
> **Nota:** Isso vai gerar o arquivo `./data/dataset_otimizado.bin` contendo a matriz clusterizada.

### 2. Compilação e Build da Imagem
Faça o build da imagem nativamente usando a ferramenta de contêiner da sua preferência:

```bash
docker build -t rinha-api .
```

### 3. Subindo a Infraestrutura Completa
O projeto acompanha um `docker-compose.yml` já configurado com as amarras de CPU e RAM oficiais da Rinha, além de um Nginx tunado para máxima concorrência.

```bash
docker-compose up -d
```

A API estará respondendo na porta **9999**:
```bash
curl -i http://localhost:9999/ready
```

---

## 📁 Estrutura de Diretórios

```text
.
├── cmd/
│   ├── api/          # Ponto de entrada da API HTTP em Golang
│   └── precompute/   # Script de Ingestão offline e clusterização IVF
├── internal/
│   └── engine/       # O Motor Híbrido CGO (core.c + core.h)
├── resources/        # Coloque o references.json aqui (Ignorado no Git)
├── data/             # Onde o dataset_otimizado.bin será gerado
├── Dockerfile        # Multi-stage build (Compila Go + CGO com -mavx2)
├── docker-compose.yml# Topologia de teste (Nginx + API 01 + API 02)
├── nginx.conf        # Configuração do Load Balancer (Keep-alive, epoll, logs off)
└── Makefile          # Orquestração de comandos de pré-processamento e compilação
```

---

## 🧪 Teste de Estresse (Gatling)

Após subir os contêineres, execute a bateria de testes oficial da Rinha de Backend apontando para `http://localhost:9999`. 

Graças à combinação do Roteador Multiplexado do Go 1.22 e da execução C-Stack, a API foi desenhada para não enfileirar requisições na porta, repassando o gargalo estritamente para os ciclos de CPU da máquina host.