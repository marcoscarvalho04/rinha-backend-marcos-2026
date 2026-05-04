# ESTÁGIO 1: Builder 
FROM golang:1.22-bookworm AS builder

# Instala as ferramentas CGO (gcc, libc, etc)
RUN apt-get update && apt-get install -y gcc make

WORKDIR /app

# Copia todo o código e os recursos (references.json) para dentro do builder
COPY . .

# 1. Roda o pré-processamento (O Go lê o JSON e gera o .bin)
RUN make precompute

# 2. Compila a API final usando o CGO
RUN make build

# ==============================================================================

# ESTÁGIO 2: Imagem Final (Magra e focada apenas em rodar a API)
FROM debian:bookworm-slim

WORKDIR /app

# Copia APENAS o executável compilado do estágio anterior
COPY --from=builder /app/bin/api .

# Copia APENAS o banco de dados binário já mastigado
COPY --from=builder /app/data/dataset_otimizado.bin ./data/

# Expõe a porta que a Rinha exige
EXPOSE 9999

# Comando para iniciar a API
CMD ["./api"]