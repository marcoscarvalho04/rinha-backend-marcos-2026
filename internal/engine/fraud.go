package engine

import "C"
import (
	"fmt"
	"unsafe"
)

// FraudResult reflete a struct search_result_t do C
type FraudResult struct {
	Score    float32
	Approved bool
}

// Init carrega a base de dados via mmap no C
func Init(filepath string) error {
	cPath := C.CString(filepath)
	defer C.free(unsafe.Pointer(cPath))

	if res := C.init_memory(cPath); res != 0 {
		return fmt.Errorf("failed to initialize C engine memory")
	}
	return nil
}

// GetFraudScore recebe um ponteiro para um array fixo de 16 floats (Zero Allocation)
// e chama a busca otimizada em C.
func GetFraudScore(vector *[16]float32) FraudResult {
	// Passamos o ponteiro do primeiro elemento diretamente para o C
	cResult := C.search_top_5((*C.float)(unsafe.Pointer(&vector[0])))

	return FraudResult{
		Score:    float32(cResult.fraud_score),
		Approved: cResult.approved != 0,
	}
}

// Close libera os recursos do C
func Close() {
	C.cleanup_memory()
}
