package main


import "C"

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
	"unsafe"
)

// --- Estruturas e Pool para Performance ---

type Transaction struct {
	Amount      float32 `json:"amount"`
	Installments int     `json:"installments"`
	RequestedAt string  `json:"requested_at"`
}

type Customer struct {
	AvgAmount      float32  `json:"avg_amount"`
	TxCount24h     int      `json:"tx_count_24h"`
	KnownMerchants []string `json:"known_merchants"`
}

type LastTransaction struct {
	Timestamp     string  `json:"timestamp"`
	KmFromCurrent float32 `json:"km_from_current"`
}

type FraudPayload struct {
	ID              string           `json:"id"`
	Transaction     Transaction      `json:"transaction"`
	Customer        Customer         `json:"customer"`
	Merchant        struct {
		ID        string  `json:"id"`
		MCC       string  `json:"mcc"`
		AvgAmount float32 `json:"avg_amount"`
	} `json:"merchant"`
	Terminal        struct {
		IsOnline   bool    `json:"is_online"`
		CardPresent bool    `json:"card_present"`
		KmFromHome  float32 `json:"km_from_home"`
	} `json:"terminal"`
	LastTransaction *LastTransaction `json:"last_transaction"`
}

// Pool para reutilizar a estrutura e evitar alocações na Heap a cada request
var payloadPool = sync.Pool{
	New: func() any {
		return new(FraudPayload)
	},
}

// --- Lógica de Apoio ---

func clamp(v float32) float32 {
	if v < 0 { return 0 }
	if v > 1 { return 1 }
	return v
}

// --- Handlers ---

func readyHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func fraudScoreHandler(w http.ResponseWriter, r *http.Request) {
	// 1. Pega estrutura do pool
	payload := payloadPool.Get().(*FraudPayload)
	defer payloadPool.Put(payload)

	// 2. Decode rápido
	// Dica: Para performance extrema, troque encoding/json por "github.com/goccy/go-json"
	if err := json.NewDecoder(r.Body).Decode(payload); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	// 3. Vetorização (As 14 dimensões conforme REGRAS_DE_DETECCAO.md)
	// Usamos 16 posições para o padding SIMD no C
	var vec [16]float32

	vec[0] = clamp(payload.Transaction.Amount / 10000.0)
	vec[1] = clamp(float32(payload.Transaction.Installments) / 12.0)
	vec[2] = clamp((payload.Transaction.Amount / payload.Customer.AvgAmount) / 10.0)
	
	t, _ := time.Parse(time.RFC3339, payload.Transaction.RequestedAt)
	vec[3] = float32(t.Hour()) / 23.0
	vec[4] = float32(t.Weekday()) / 6.0

	if payload.LastTransaction == nil {
		vec[5] = -1.0
		vec[6] = -1.0
	} else {
		lastT, _ := time.Parse(time.RFC3339, payload.LastTransaction.Timestamp)
		diffMin := float32(t.Sub(lastT).Minutes())
		vec[5] = clamp(diffMin / 1440.0)
		vec[6] = clamp(payload.LastTransaction.KmFromCurrent / 1000.0)
	}

	vec[7] = clamp(payload.Terminal.KmFromHome / 1000.0)
	vec[8] = clamp(float32(payload.Customer.TxCount24h) / 20.0)
	
	if payload.Terminal.IsOnline { vec[9] = 1.0 } else { vec[9] = 0 }
	if payload.Terminal.CardPresent { vec[10] = 1.0 } else { vec[10] = 0 }
	
	// Exemplo simplificado de busca de mercante
	isUnknown := 1.0
	for _, m := range payload.Customer.KnownMerchants {
		if m == payload.Merchant.ID {
			isUnknown = 0.0
			break
		}
	}
	vec[11] = float32(isUnknown)
	vec[12] = 0.5 // TODO: Implementar lookup no mcc_risk.json
	vec[13] = clamp(payload.Merchant.AvgAmount / 10000.0)
	
	// Posições 14 e 15 são padding (0.0)

	// 4. Chamada CGO para o motor em C
	cResult := C.search_top_5((*C.float)(unsafe.Pointer(&vec[0])))

	// 5. Resposta
	score := float32(cResult.score)
	response := map[string]any{
		"approved":    score < 0.6,
		"fraud_score": score,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func main() {
	// Inicialização do C (mmap da base de dados)
	// O C precisa carregar o arquivo binário gerado no pre-caching
	cPath := C.CString("./resources/dataset_otimizado.bin")
	defer C.free(unsafe.Pointer(cPath))
	
	// TODO: C.init_memory(cPath) na implementação do core.c
	fmt.Println("🚀 Memória mapeada via mmap. Motor C pronto.")

	// Configuração do Router minimalista (Go 1.22+)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ready", readyHandler)
	mux.HandleFunc("POST /fraud-score", fraudScoreHandler)

	server := &http.Server{
		Addr:         ":9999",
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	fmt.Println("🔥 API de Detecção de Fraude rodando na porta 9999")
	log.Fatal(server.ListenAndServe())
}