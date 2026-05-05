# Variáveis de ambiente para forçar otimização extrema no compilador C (GCC/Clang)
# -O3: Nível máximo de otimização segura.
# -march=native: Otimiza o binário especificamente para a arquitetura da CPU que está compilando (liberando as instruções AVX2/SIMD).
export CGO_CFLAGS=-O3 -mavx2
export CGO_ENABLED=1

.PHONY: all precompute build run clean

# Comando padrão ao digitar apenas "make"
all: clean precompute build

# 1. Gera o banco de dados em memória (o arquivo .bin)
precompute:
	@echo "🔥 [1/3] Rodando o Pré-processamento..."
	go run ./cmd/precompute/

# 2. Compila a API Go + Motor C
# -ldflags="-s -w" remove informações de debug do binário Go, deixando ele muito menor e mais rápido de carregar.
build:
	@echo "🔨 [2/3] Compilando a API (Go + CGO)..."
	@mkdir -p bin
	go build -ldflags="-s -w" -o bin/api ./cmd/api/
	@echo "✅ Build concluído: ./bin/api"

# 3. Roda o binário final gerado
run: build
	@echo "🚀 [3/3] Subindo a API para a Rinha..."
	./bin/api

# Limpa os binários e a base de dados gerada
clean:
	@echo "🧹 Limpando artefatos..."
	rm -rf bin/
	rm -rf data/