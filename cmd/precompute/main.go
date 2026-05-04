package main

import (
	"encoding/json"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"time"
)

// Constantes Oficiais da Documentação
const (
	MaxAmount             = 10000.0
	MaxInstallments       = 12.0
	AmountVsAvgRatio      = 10.0
	MaxMinutes            = 1440.0
	MaxKm                 = 1000.0
	MaxTxCount24h         = 20.0
	MaxMerchantAvgAmount  = 10000.0
	NumClusters           = 1024 // Para 3M de records
	VectorDim             = 16   // 14 + 2 padding
)

type ReferenceRecord struct {
	ID          string `json:"id"`
	IsFraud     bool   `json:"is_fraud"` // Campo extra no dataset de referência
	Transaction struct {
		Amount      float32 `json:"amount"`
		Installments int     `json:"installments"`
		RequestedAt string  `json:"requested_at"`
	} `json:"transaction"`
	Customer struct {
		AvgAmount      float32  `json:"avg_amount"`
		TxCount24h     int      `json:"tx_count_24h"`
		KnownMerchants []string `json:"known_merchants"`
	} `json:"customer"`
	Merchant struct {
		ID        string  `json:"id"`
		MCC       string  `json:"mcc"`
		AvgAmount float32 `json:"avg_amount"`
	} `json:"merchant"`
	Terminal struct {
		IsOnline    bool    `json:"is_online"`
		CardPresent bool    `json:"card_present"`
		KmFromHome  float32 `json:"km_from_home"`
	} `json:"terminal"`
	LastTransaction *struct {
		Timestamp     string  `json:"timestamp"`
		KmFromCurrent float32 `json:"km_from_current"`
	} `json:"last_transaction"`
}

func main() {
	fmt.Println("📖 Lendo references.json...")
	file, _ := os.Open("./resources/references.json")
	defer file.Close()

	var records []ReferenceRecord
	json.NewDecoder(file).Decode(&records)

	total := len(records)
	fmt.Printf("✅ %d registos carregados. Iniciando vetorização oficial...\n", total)

	vectors := make([][16]float32, total)
	labels := make([]uint8, total)

	for i, r := range records {
		v := vectorize(r)
		vectors[i] = v
		if r.IsFraud { labels[i] = 1 } else { labels[i] = 0 }
	}

	// Clusterização IVF (K-Means simplificado para 1 iteração ou centroids aleatórios)
	fmt.Println("🏙️  Agrupando em clusters (IVF)...")
	centroids := make([][16]float32, NumClusters)
	for i := 0; i < NumClusters; i++ {
		centroids[i] = vectors[i * (total/NumClusters)] 
	}

	buckets := make([][]int, NumClusters)
	for i, v := range vectors {
		bestC := 0
		minD := float32(math.MaxFloat32)
		for cID, cV := range centroids {
			d := distSq(v, cV)
			if d < minD {
				minD = d
				bestC = cID
			}
		}
		buckets[bestC] = append(buckets[bestC], i)
	}

	fmt.Println("Salvando arquivo binário...")

	// Salva o binário otimizado
	saveOptimizedBinary(centroids, buckets, vectors, labels)

	fmt.Println("✅ Arquivo binário otimizado criado!")
}

func vectorize(r ReferenceRecord) [16]float32 {
	var v [16]float32
	
	// Dim 0: Amount
	v[0] = float32(math.Min(float64(r.Transaction.Amount/MaxAmount), 1.0))
	// Dim 1: Installments
	v[1] = float32(math.Min(float64(float32(r.Transaction.Installments)/MaxInstallments), 1.0))
	// Dim 2: Amount vs Avg
	v[2] = float32(math.Min(float64((r.Transaction.Amount/r.Customer.AvgAmount)/AmountVsAvgRatio), 1.0))
	
	t, _ := time.Parse(time.RFC3339, r.Transaction.RequestedAt)
	// Dim 3: Hour
	v[3] = float32(t.Hour()) / 23.0
	// Dim 4: Day of Week
	v[4] = float32(t.Weekday()) / 6.0

	// Dim 5 & 6: Last Transaction
	if r.LastTransaction == nil {
		v[5] = -1.0
		v[6] = -1.0
	} else {
		lt, _ := time.Parse(time.RFC3339, r.LastTransaction.Timestamp)
		diff := float32(t.Sub(lt).Minutes())
		v[5] = float32(math.Min(float64(diff/MaxMinutes), 1.0))
		v[6] = float32(math.Min(float64(r.LastTransaction.KmFromCurrent/MaxKm), 1.0))
	}

	v[7] = float32(math.Min(float64(r.Terminal.KmFromHome/MaxKm), 1.0))
	v[8] = float32(math.Min(float64(float32(r.Customer.TxCount24h)/MaxTxCount24h), 1.0))
	
	if r.Terminal.IsOnline { v[9] = 1.0 } else { v[9] = 0 }
	if r.Terminal.CardPresent { v[10] = 1.0 } else { v[10] = 0 }

	isUnknown := 1.0
	for _, m := range r.Customer.KnownMerchants {
		if m == r.Merchant.ID { isUnknown = 0.0; break }
	}
	v[11] = float32(isUnknown)
	v[12] = 0.5 // Risco do MCC (Idealmente ler do mcc_risk.json)
	v[13] = float32(math.Min(float64(r.Merchant.AvgAmount/MaxMerchantAvgAmount), 1.0))

	return v
}

func distSq(a, b [16]float32) float32 {
	var s float32
	for i := 0; i < 14; i++ {
		d := a[i] - b[i]
		s += d * d
	}
	return s
}

func saveOptimizedBinary(centroids [][16]float32, buckets [][]int, allVectors [][16]float32, labels []uint8) {
	f, err := os.Create("./resources/dataset_otimizado.bin")
	defer f.Close()
	if err != nil {
		panic("Erro ao criar arquivo: %v", err)
	}
	// 1. Escreve Centróides
	binary.Write(f, binary.LittleEndian, centroids)

	// 2. Escreve Offsets
	offsets := make([]uint32, NumClusters+1)
	curr := uint32(0)
	for i, b := range buckets {
		offsets[i] = curr
		curr += uint32(len(b))
	}
	offsets[NumClusters] = curr
	binary.Write(f, binary.LittleEndian, offsets)

	// 3. Escreve Vetores reordenados por bucket
	for _, b := range buckets {
		for _, idx := range b {
			binary.Write(f, binary.LittleEndian, allVectors[idx])
		}
	}

	// 4. Escreve Labels reordenados
	for _, b := range buckets {
		for _, idx := range b {
			binary.Write(f, binary.LittleEndian, labels[idx])
		}
	}
}