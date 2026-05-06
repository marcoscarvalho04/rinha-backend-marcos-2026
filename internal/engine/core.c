#include "core.h"
#include <immintrin.h>
#include <stdint.h>
#include <stddef.h>
#include <stdio.h>
#include <stdlib.h>
#include <fcntl.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <unistd.h>

#define NUM_CLUSTERS  2048
#define VECTOR_DIM    16
#define NPROBE        10
#define PREFETCH_DIST  8  // vetores à frente para pré-carregar (8 × 32 bytes = 256 bytes)

// --- Ponteiros Globais do Banco em Memória (Zero-Copy via mmap) ---
float*    centroids = NULL;
uint32_t* bucket_offsets = NULL;
uint16_t* all_vectors = NULL;  // float16 para economizar memória (~metade do tamanho)
uint8_t*  all_flags = NULL;
size_t    db_total_records = 0;

// --- Função Auxiliar SIMD: Soma horizontal de um registrador de 256 bits ---
static inline float horizontal_add_m256(__m256 v) {
    __m128 vlow  = _mm256_castps256_ps128(v);
    __m128 vhigh = _mm256_extractf128_ps(v, 1);
    vlow  = _mm_add_ps(vlow, vhigh);
    __m128 shuf  = _mm_movehdup_ps(vlow);
    __m128 sums  = _mm_add_ps(vlow, shuf);
    shuf         = _mm_movehl_ps(shuf, sums);
    sums         = _mm_add_ss(sums, shuf);
    return _mm_cvtss_f32(sums);
}

// --- Mapeamento do Arquivo para a RAM ---
void init_memory(const char* filepath) {
    int fd = open(filepath, O_RDONLY);
    if (fd == -1) {
        perror("[FATAL] Erro ao abrir dataset_otimizado.bin");
        exit(EXIT_FAILURE);
    }

    struct stat sb;
    if (fstat(fd, &sb) == -1) {
        perror("[FATAL] Erro ao pegar tamanho do arquivo");
        exit(EXIT_FAILURE);
    }

    void* mapped_data = mmap(NULL, sb.st_size, PROT_READ, MAP_SHARED, fd, 0);
    if (mapped_data == MAP_FAILED) {
        perror("[FATAL] Erro no mmap");
        exit(EXIT_FAILURE);
    }

    char* base = (char*)mapped_data;

    // 1. Centróides: 1024 * 16 floats
    centroids = (float*)base;
    size_t offset_centroids = NUM_CLUSTERS * VECTOR_DIM * sizeof(float);

    // 2. Offsets dos buckets: 1025 inteiros de 32 bits
    bucket_offsets = (uint32_t*)(base + offset_centroids);
    size_t offset_buckets = offset_centroids + ((NUM_CLUSTERS + 1) * sizeof(uint32_t));

    // O último offset guarda exatamente o número total de registros armazenados
    db_total_records = bucket_offsets[NUM_CLUSTERS];

    // 3. Matriz de Vetores: N * 16 float16 (uint16) — metade do tamanho vs float32
    all_vectors = (uint16_t*)(base + offset_buckets);
    size_t offset_vectors = offset_buckets + (db_total_records * VECTOR_DIM * sizeof(uint16_t));

    // 4. Flags de Fraude: N bytes
    all_flags = (uint8_t*)(base + offset_vectors);

    close(fd); // O arquivo pode ser fechado após o mmap

    printf("🔧 [Motor C SIMD IVF] Inicializado com sucesso!\n");
    printf("   -> Clusters: %d\n", NUM_CLUSTERS);
    printf("   -> Total de Registros: %zu\n", db_total_records);
}

// --- O Coração da Performance: Busca Vetorial SIMD com K-Means ---
SearchResult search_top_5(float* target) {
    // Carrega o vetor alvo para a CPU (2 instruções AVX para 16 floats)
    __m256 t_0_7 = _mm256_loadu_ps(target);
    __m256 t_8_15 = _mm256_loadu_ps(target + 8);

    // ==========================================
    // FASE 1: Encontrar os NPROBE centróides mais próximos
    // ==========================================
    float probe_dists[NPROBE];
    int   probe_ids[NPROBE];
    for (int i = 0; i < NPROBE; i++) {
        probe_dists[i] = 1e9f;
        probe_ids[i]   = 0;
    }

    for (int i = 0; i < NUM_CLUSTERS; i++) {
        float* c_ptr = centroids + (i * VECTOR_DIM);
        __m256 c_0_7  = _mm256_loadu_ps(c_ptr);
        __m256 c_8_15 = _mm256_loadu_ps(c_ptr + 8);

        __m256 diff_0_7  = _mm256_sub_ps(t_0_7,  c_0_7);
        __m256 diff_8_15 = _mm256_sub_ps(t_8_15, c_8_15);
        __m256 sq_0_7    = _mm256_mul_ps(diff_0_7,  diff_0_7);
        __m256 sq_8_15   = _mm256_mul_ps(diff_8_15, diff_8_15);
        float dist = horizontal_add_m256(_mm256_add_ps(sq_0_7, sq_8_15));

        if (dist < probe_dists[NPROBE - 1]) {
            int pos = NPROBE - 2;
            while (pos >= 0 && probe_dists[pos] > dist) {
                probe_dists[pos + 1] = probe_dists[pos];
                probe_ids[pos + 1]   = probe_ids[pos];
                pos--;
            }
            probe_dists[pos + 1] = dist;
            probe_ids[pos + 1]   = i;
        }
    }

    // ==========================================
    // FASE 2: Busca nos NPROBE buckets, top-5 global
    // ==========================================
    float top5_dists[5] = {1e9f, 1e9f, 1e9f, 1e9f, 1e9f};
    uint8_t top5_fraud[5] = {0, 0, 0, 0, 0};

    for (int p = 0; p < NPROBE; p++) {
        uint32_t start_idx = bucket_offsets[probe_ids[p]];
        uint32_t end_idx   = bucket_offsets[probe_ids[p] + 1];

        for (uint32_t i = start_idx; i < end_idx; i++) {
            // Pré-carrega o vetor PREFETCH_DIST posições à frente para esconder latência de RAM
            __builtin_prefetch(all_vectors + ((i + PREFETCH_DIST) * VECTOR_DIM), 0, 1);

            uint16_t* v_ptr = all_vectors + (i * VECTOR_DIM);

            // Carrega 8 float16 em 128 bits e converte para 8 float32 em 256 bits (F16C)
            __m256 v_0_7  = _mm256_cvtph_ps(_mm_loadu_si128((__m128i*)(v_ptr)));
            __m256 v_8_15 = _mm256_cvtph_ps(_mm_loadu_si128((__m128i*)(v_ptr + 8)));

            __m256 diff_0_7  = _mm256_sub_ps(t_0_7,  v_0_7);
            __m256 diff_8_15 = _mm256_sub_ps(t_8_15, v_8_15);
            __m256 sq_0_7    = _mm256_mul_ps(diff_0_7,  diff_0_7);
            __m256 sq_8_15   = _mm256_mul_ps(diff_8_15, diff_8_15);
            float dist = horizontal_add_m256(_mm256_add_ps(sq_0_7, sq_8_15));

            if (dist < top5_dists[4]) {
                int pos = 3;
                while (pos >= 0 && top5_dists[pos] > dist) {
                    top5_dists[pos + 1] = top5_dists[pos];
                    top5_fraud[pos + 1] = top5_fraud[pos];
                    pos--;
                }
                top5_dists[pos + 1] = dist;
                top5_fraud[pos + 1] = all_flags[i];
            }
        }
    }

    // ==========================================
    // FASE 3: Cálculo do Fraud Score Final
    // ==========================================
    float frauds_found = 0.0f;
    for (int i = 0; i < 5; i++) {
        frauds_found += (float)top5_fraud[i];
    }

    SearchResult result;
    result.score = frauds_found / 5.0f;
    return result;
}