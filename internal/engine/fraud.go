package engine

/*
#cgo CFLAGS: -O3 -mavx2
#include <stdlib.h>
#include "core.h"
*/
import "C"
import "unsafe"

type FraudResult struct {
	Score    float32
	Approved bool
}

func Init(filepath string) {
	cPath := C.CString(filepath)
	defer C.free(unsafe.Pointer(cPath))
	C.init_memory(cPath)
}

func GetFraudScore(vector *[16]float32) FraudResult {
	cResult := C.search_top_5((*C.float)(unsafe.Pointer(&vector[0])))
	score := float32(cResult.score)
	return FraudResult{
		Score:    score,
		Approved: score < 0.6,
	}
}
