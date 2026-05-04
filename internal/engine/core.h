#ifndef CORE_H
#define CORE_H

#include <stdint.h>

// Estrutura de retorno para o Go
typedef struct {
    float score;
} SearchResult;

// Funções exportadas para o CGO
void init_memory(const char* filepath);
SearchResult search_top_5(float* target);

#endif // CORE_H