package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"
)

// --- Casos de teste com resultado esperado ---

type TestCase struct {
	Name        string
	ExpectFraud bool
	Payload     string
}

var cases = []TestCase{
	{
		Name:        "fraude_alta_quantia_mcc_risco",
		ExpectFraud: true,
		Payload: `{
			"id": "tx-bench-001",
			"transaction": { "amount": 9500.00, "installments": 1, "requested_at": "2026-03-11T03:15:00Z" },
			"customer": { "avg_amount": 95.00, "tx_count_24h": 12, "known_merchants": ["MERC-001"] },
			"merchant": { "id": "MERC-999", "mcc": "7995", "avg_amount": 8000.00 },
			"terminal": { "is_online": true, "card_present": false, "km_from_home": 850.0 },
			"last_transaction": null
		}`,
	},
	{
		Name:        "fraude_mcc_7802_mercante_desconhecido",
		ExpectFraud: true,
		Payload: `{
			"id": "tx-bench-002",
			"transaction": { "amount": 4125.00, "installments": 1, "requested_at": "2026-03-11T02:30:00Z" },
			"customer": { "avg_amount": 82.00, "tx_count_24h": 8, "known_merchants": ["MERC-003", "MERC-016"] },
			"merchant": { "id": "MERC-777", "mcc": "7802", "avg_amount": 3500.00 },
			"terminal": { "is_online": true, "card_present": false, "km_from_home": 620.0 },
			"last_transaction": null
		}`,
	},
	{
		Name:        "fraude_multiplas_tx_24h",
		ExpectFraud: true,
		Payload: `{
			"id": "tx-bench-003",
			"transaction": { "amount": 750.00, "installments": 1, "requested_at": "2026-03-11T04:00:00Z" },
			"customer": { "avg_amount": 60.00, "tx_count_24h": 19, "known_merchants": ["MERC-010"] },
			"merchant": { "id": "MERC-888", "mcc": "7801", "avg_amount": 700.00 },
			"terminal": { "is_online": true, "card_present": false, "km_from_home": 400.0 },
			"last_transaction": null
		}`,
	},
	{
		Name:        "legit_supermercado_proximo",
		ExpectFraud: false,
		Payload: `{
			"id": "tx-bench-004",
			"transaction": { "amount": 87.50, "installments": 1, "requested_at": "2026-03-11T12:30:00Z" },
			"customer": { "avg_amount": 95.00, "tx_count_24h": 2, "known_merchants": ["MERC-003", "MERC-016"] },
			"merchant": { "id": "MERC-016", "mcc": "5411", "avg_amount": 90.00 },
			"terminal": { "is_online": false, "card_present": true, "km_from_home": 3.5 },
			"last_transaction": { "timestamp": "2026-03-10T18:00:00Z", "km_from_current": 2.1 }
		}`,
	},
	{
		Name:        "legit_farmacia_parcelado",
		ExpectFraud: false,
		Payload: `{
			"id": "tx-bench-005",
			"transaction": { "amount": 210.00, "installments": 3, "requested_at": "2026-03-11T10:15:00Z" },
			"customer": { "avg_amount": 180.00, "tx_count_24h": 1, "known_merchants": ["MERC-003", "MERC-020"] },
			"merchant": { "id": "MERC-003", "mcc": "5912", "avg_amount": 200.00 },
			"terminal": { "is_online": false, "card_present": true, "km_from_home": 12.0 },
			"last_transaction": { "timestamp": "2026-03-11T08:00:00Z", "km_from_current": 10.5 }
		}`,
	},
	{
		Name:        "legit_restaurante_horario_comercial",
		ExpectFraud: false,
		Payload: `{
			"id": "tx-bench-006",
			"transaction": { "amount": 45.90, "installments": 1, "requested_at": "2026-03-11T13:00:00Z" },
			"customer": { "avg_amount": 55.00, "tx_count_24h": 3, "known_merchants": ["MERC-050", "MERC-051"] },
			"merchant": { "id": "MERC-050", "mcc": "5812", "avg_amount": 50.00 },
			"terminal": { "is_online": false, "card_present": true, "km_from_home": 8.0 },
			"last_transaction": { "timestamp": "2026-03-11T07:30:00Z", "km_from_current": 7.0 }
		}`,
	},
}

// --- Resultado por requisição ---

type Result struct {
	CaseName    string
	ExpectFraud bool
	FraudScore  float64
	Approved    bool
	Latency     time.Duration
	Err         error
}

func (r Result) correct() bool {
	if r.Err != nil {
		return false
	}
	gotFraud := !r.Approved
	return gotFraud == r.ExpectFraud
}

// --- Execução ---

func runRequest(client *http.Client, url string, tc TestCase) Result {
	start := time.Now()
	resp, err := client.Post(url, "application/json", bytes.NewBufferString(tc.Payload))
	latency := time.Since(start)

	if err != nil {
		return Result{CaseName: tc.Name, ExpectFraud: tc.ExpectFraud, Latency: latency, Err: err}
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var out struct {
		FraudScore float64 `json:"fraud_score"`
		Approved   bool    `json:"approved"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return Result{CaseName: tc.Name, ExpectFraud: tc.ExpectFraud, Latency: latency, Err: err}
	}

	return Result{
		CaseName:    tc.Name,
		ExpectFraud: tc.ExpectFraud,
		FraudScore:  out.FraudScore,
		Approved:    out.Approved,
		Latency:     latency,
	}
}

// --- Main ---

func main() {
	url := flag.String("url", "http://localhost:9999/fraud-score", "endpoint alvo")
	n := flag.Int("n", 200, "requisições por caso de teste")
	c := flag.Int("c", 50, "concorrência (goroutines simultâneas)")
	flag.Parse()

	totalRequests := len(cases) * *n
	results := make([]Result, 0, totalRequests)
	var mu sync.Mutex

	client := &http.Client{Timeout: 10 * time.Second}

	sem := make(chan struct{}, *c)
	var wg sync.WaitGroup

	fmt.Printf("🔥 Benchmark iniciado: %d casos × %d requisições = %d total | concorrência: %d\n\n",
		len(cases), *n, totalRequests, *c)

	globalStart := time.Now()

	for _, tc := range cases {
		for i := 0; i < *n; i++ {
			wg.Add(1)
			sem <- struct{}{}
			go func(tc TestCase) {
				defer wg.Done()
				defer func() { <-sem }()
				r := runRequest(client, *url, tc)
				mu.Lock()
				results = append(results, r)
				mu.Unlock()
			}(tc)
		}
	}

	wg.Wait()
	totalElapsed := time.Since(globalStart)

	// --- Agregação ---

	type CaseSummary struct {
		latencies []time.Duration
		correct   int
		errors    int
		scores    []float64
	}

	summaries := map[string]*CaseSummary{}
	for _, tc := range cases {
		summaries[tc.Name] = &CaseSummary{}
	}

	var allLatencies []time.Duration
	totalErrors, totalCorrect := 0, 0

	for _, r := range results {
		s := summaries[r.CaseName]
		if r.Err != nil {
			s.errors++
			totalErrors++
			continue
		}
		s.latencies = append(s.latencies, r.Latency)
		s.scores = append(s.scores, r.FraudScore)
		allLatencies = append(allLatencies, r.Latency)
		if r.correct() {
			s.correct++
			totalCorrect++
		}
	}

	pct := func(latencies []time.Duration, p float64) time.Duration {
		if len(latencies) == 0 {
			return 0
		}
		sorted := make([]time.Duration, len(latencies))
		copy(sorted, latencies)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		idx := int(float64(len(sorted)-1) * p)
		return sorted[idx]
	}

	avgScore := func(scores []float64) float64 {
		if len(scores) == 0 {
			return 0
		}
		var sum float64
		for _, s := range scores {
			sum += s
		}
		return sum / float64(len(scores))
	}

	// --- Relatório por caso ---

	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Printf("%-38s %7s %7s %7s %7s %8s %6s\n",
		"CASO", "p50", "p95", "p99", "score", "acerto", "erros")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	for _, tc := range cases {
		s := summaries[tc.Name]
		total := len(s.latencies)
		acerto := 0.0
		if total > 0 {
			acerto = float64(s.correct) / float64(total) * 100
		}
		label := "legit"
		if tc.ExpectFraud {
			label = "fraud"
		}
		fmt.Printf("%-30s [%5s] %7s %7s %7s %7.3f %7.1f%% %6d\n",
			tc.Name, label,
			pct(s.latencies, 0.50).Round(time.Microsecond),
			pct(s.latencies, 0.95).Round(time.Microsecond),
			pct(s.latencies, 0.99).Round(time.Microsecond),
			avgScore(s.scores),
			acerto,
			s.errors,
		)
	}

	// --- Relatório global ---

	successful := totalRequests - totalErrors
	accuracy := 0.0
	if successful > 0 {
		accuracy = float64(totalCorrect) / float64(successful) * 100
	}
	rps := float64(totalRequests) / totalElapsed.Seconds()

	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Printf("\n📊 RESUMO GLOBAL\n")
	fmt.Printf("   Total de requisições : %d\n", totalRequests)
	fmt.Printf("   Erros                : %d\n", totalErrors)
	fmt.Printf("   Tempo total          : %s\n", totalElapsed.Round(time.Millisecond))
	fmt.Printf("   Throughput           : %.0f req/s\n", rps)
	fmt.Printf("   Latência p50         : %s\n", pct(allLatencies, 0.50).Round(time.Microsecond))
	fmt.Printf("   Latência p95         : %s\n", pct(allLatencies, 0.95).Round(time.Microsecond))
	fmt.Printf("   Latência p99         : %s\n", pct(allLatencies, 0.99).Round(time.Microsecond))
	fmt.Printf("   Acurácia             : %.1f%% (%d/%d corretos)\n", accuracy, totalCorrect, successful)
}
