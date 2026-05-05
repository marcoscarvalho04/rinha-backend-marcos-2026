package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"sync"
	"time"

	gojson "github.com/goccy/go-json"
	"github.com/valyala/fasthttp"

	"github.com/marcos-codes/rinha-2026/internal/engine"
)

// --- Configuração carregada em startup ---

type NormConfig struct {
	MaxAmount            float32 `json:"max_amount"`
	MaxInstallments      float32 `json:"max_installments"`
	AmountVsAvgRatio     float32 `json:"amount_vs_avg_ratio"`
	MaxMinutes           float32 `json:"max_minutes"`
	MaxKm                float32 `json:"max_km"`
	MaxTxCount24h        float32 `json:"max_tx_count_24h"`
	MaxMerchantAvgAmount float32 `json:"max_merchant_avg_amount"`
}

var (
	norm    NormConfig
	mccRisk map[string]float32
)

func loadConfig() {
	normData, err := os.ReadFile("./resources/normalization.json")
	if err != nil {
		log.Fatalf("Erro ao ler normalization.json: %v", err)
	}
	if err := gojson.Unmarshal(normData, &norm); err != nil {
		log.Fatalf("Erro ao decodificar normalization.json: %v", err)
	}

	mccData, err := os.ReadFile("./resources/mcc_risk.json")
	if err != nil {
		log.Fatalf("Erro ao ler mcc_risk.json: %v", err)
	}
	if err := gojson.Unmarshal(mccData, &mccRisk); err != nil {
		log.Fatalf("Erro ao decodificar mcc_risk.json: %v", err)
	}

	fmt.Printf("✅ Configuração carregada: %d MCCs conhecidos.\n", len(mccRisk))
}

func mccRiskFor(mcc string) float32 {
	if r, ok := mccRisk[mcc]; ok {
		return r
	}
	return 0.5
}

// --- Estruturas e Pool ---

type Transaction struct {
	Amount       float32 `json:"amount"`
	Installments int     `json:"installments"`
	RequestedAt  string  `json:"requested_at"`
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

type Merchant struct {
	ID        string  `json:"id"`
	MCC       string  `json:"mcc"`
	AvgAmount float32 `json:"avg_amount"`
}

type Terminal struct {
	IsOnline    bool    `json:"is_online"`
	CardPresent bool    `json:"card_present"`
	KmFromHome  float32 `json:"km_from_home"`
}

type FraudPayload struct {
	ID              string           `json:"id"`
	Transaction     Transaction      `json:"transaction"`
	Customer        Customer         `json:"customer"`
	Merchant        Merchant         `json:"merchant"`
	Terminal        Terminal         `json:"terminal"`
	LastTransaction *LastTransaction `json:"last_transaction"`
}

type FraudResponse struct {
	Approved   bool    `json:"approved"`
	FraudScore float32 `json:"fraud_score"`
}

var payloadPool = sync.Pool{
	New: func() any { return new(FraudPayload) },
}

// content-type pré-alocado para evitar conversão string→[]byte por request
var contentTypeJSON = []byte("application/json")

// --- Parsing de tempo sem alocação ---

func parseRFC3339Hour(s string) float32 {
	if len(s) < 13 {
		return 0
	}
	h := int(s[11]-'0')*10 + int(s[12]-'0')
	return float32(h) / 23.0
}

func parseRFC3339Weekday(s string) float32 {
	if len(s) < 10 {
		return 0
	}
	y := int(s[0]-'0')*1000 + int(s[1]-'0')*100 + int(s[2]-'0')*10 + int(s[3]-'0')
	m := int(s[5]-'0')*10 + int(s[6]-'0')
	d := int(s[8]-'0')*10 + int(s[9]-'0')
	t := []int{0, 3, 2, 5, 0, 3, 5, 1, 4, 6, 2, 4}
	if m < 3 {
		y--
	}
	w := (y + y/4 - y/100 + y/400 + t[m-1] + d) % 7
	return float32(w) / 6.0
}

func diffMinutes(from, to string) float32 {
	t1, err1 := time.Parse(time.RFC3339, from)
	t2, err2 := time.Parse(time.RFC3339, to)
	if err1 != nil || err2 != nil {
		return 0
	}
	return float32(t2.Sub(t1).Minutes())
}

// --- Handlers ---

func clamp(v float32) float32 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func router(ctx *fasthttp.RequestCtx) {
	switch string(ctx.Path()) {
	case "/ready":
		if ctx.IsGet() {
			ctx.SetStatusCode(fasthttp.StatusOK)
		}
	case "/fraud-score":
		if ctx.IsPost() {
			fraudScoreHandler(ctx)
		}
	default:
		ctx.SetStatusCode(fasthttp.StatusNotFound)
	}
}

func fraudScoreHandler(ctx *fasthttp.RequestCtx) {
	payload := payloadPool.Get().(*FraudPayload)
	payload.Customer.KnownMerchants = payload.Customer.KnownMerchants[:0]
	payload.LastTransaction = nil
	defer payloadPool.Put(payload)

	// PostBody() é zero-copy — aponta para o buffer interno do fasthttp
	if err := gojson.Unmarshal(ctx.PostBody(), payload); err != nil {
		ctx.Error("invalid payload", fasthttp.StatusBadRequest)
		return
	}

	var vec [16]float32

	vec[0] = clamp(payload.Transaction.Amount / norm.MaxAmount)
	vec[1] = clamp(float32(payload.Transaction.Installments) / norm.MaxInstallments)
	vec[2] = clamp((payload.Transaction.Amount / payload.Customer.AvgAmount) / norm.AmountVsAvgRatio)
	vec[3] = parseRFC3339Hour(payload.Transaction.RequestedAt)
	vec[4] = parseRFC3339Weekday(payload.Transaction.RequestedAt)

	if payload.LastTransaction == nil {
		vec[5] = -1.0
		vec[6] = -1.0
	} else {
		diff := diffMinutes(payload.LastTransaction.Timestamp, payload.Transaction.RequestedAt)
		vec[5] = clamp(diff / norm.MaxMinutes)
		vec[6] = clamp(payload.LastTransaction.KmFromCurrent / norm.MaxKm)
	}

	vec[7] = clamp(payload.Terminal.KmFromHome / norm.MaxKm)
	vec[8] = clamp(float32(payload.Customer.TxCount24h) / norm.MaxTxCount24h)

	if payload.Terminal.IsOnline {
		vec[9] = 1.0
	}
	if payload.Terminal.CardPresent {
		vec[10] = 1.0
	}

	isUnknown := float32(1.0)
	for _, m := range payload.Customer.KnownMerchants {
		if m == payload.Merchant.ID {
			isUnknown = 0.0
			break
		}
	}
	vec[11] = isUnknown
	vec[12] = mccRiskFor(payload.Merchant.MCC)
	vec[13] = clamp(payload.Merchant.AvgAmount / norm.MaxMerchantAvgAmount)

	result := engine.GetFraudScore(&vec)

	ctx.SetContentTypeBytes(contentTypeJSON)
	gojson.NewEncoder(ctx).Encode(FraudResponse{
		Approved:   result.Approved,
		FraudScore: result.Score,
	})
}

func main() {
	loadConfig()
	engine.Init("./resources/dataset_otimizado.bin")
	fmt.Println("🚀 Memória mapeada via mmap. Motor C pronto.")

	server := &fasthttp.Server{
		Handler:      router,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	if socketPath := os.Getenv("SOCKET_PATH"); socketPath != "" {
		os.Remove(socketPath)
		ln, err := net.Listen("unix", socketPath)
		if err != nil {
			log.Fatalf("Erro ao criar unix socket %s: %v", socketPath, err)
		}
		os.Chmod(socketPath, 0777)
		defer os.Remove(socketPath)
		fmt.Printf("🔥 API via unix socket: %s\n", socketPath)
		log.Fatal(server.Serve(ln))
	} else {
		fmt.Println("🔥 API de Detecção de Fraude rodando na porta 9999")
		log.Fatal(server.ListenAndServe(":9999"))
	}
}
