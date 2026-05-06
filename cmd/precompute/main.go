package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"runtime"
	"sync"
)

const (
	NumClusters = 1024
	VectorDim   = 16
	KMeansIter  = 15
)

type ReferenceRecord struct {
	Vector [14]float32 `json:"vector"`
	Label  string      `json:"label"` // "legit" ou "fraud"
}

func main() {
	fmt.Println("📖 Lendo references.json...")
	file, err := os.Open("./resources/references.json")
	if err != nil {
		panic("Erro ao abrir references.json: " + err.Error())
	}
	defer file.Close()

	var records []ReferenceRecord
	if err := json.NewDecoder(file).Decode(&records); err != nil {
		panic("Erro ao decodificar references.json: " + err.Error())
	}

	total := len(records)
	fmt.Printf("✅ %d registros carregados.\n", total)

	vectors := make([][16]float32, total)
	labels := make([]uint8, total)

	for i, r := range records {
		copy(vectors[i][:14], r.Vector[:])
		if r.Label == "fraud" {
			labels[i] = 1
		}
	}

	fmt.Printf("🏙️  Executando K-Means (%d clusters, %d iterações, %d workers)...\n",
		NumClusters, KMeansIter, runtime.NumCPU())

	centroids := initCentroids(vectors)
	for iter := 0; iter < KMeansIter; iter++ {
		assignments := assignParallel(vectors, centroids)
		centroids = updateCentroids(vectors, assignments)
		fmt.Printf("   Iteração %d/%d concluída.\n", iter+1, KMeansIter)
	}

	// Atribuição final para montar os buckets
	assignments := assignParallel(vectors, centroids)
	buckets := make([][]int, NumClusters)
	for i, c := range assignments {
		buckets[c] = append(buckets[c], i)
	}

	fmt.Println("💾 Salvando dataset_otimizado.bin...")
	saveOptimizedBinary(centroids, buckets, vectors, labels)
	fmt.Println("✅ dataset_otimizado.bin criado com sucesso!")
}

// initCentroids escolhe NumClusters vetores aleatórios como centróides iniciais.
func initCentroids(vectors [][16]float32) [][16]float32 {
	perm := rand.Perm(len(vectors))
	centroids := make([][16]float32, NumClusters)
	for i := 0; i < NumClusters; i++ {
		centroids[i] = vectors[perm[i]]
	}
	return centroids
}

// assignParallel atribui cada vetor ao centróide mais próximo usando todos os CPUs.
func assignParallel(vectors [][16]float32, centroids [][16]float32) []int {
	assignments := make([]int, len(vectors))
	nWorkers := runtime.NumCPU()
	chunkSize := (len(vectors) + nWorkers - 1) / nWorkers

	var wg sync.WaitGroup
	for w := 0; w < nWorkers; w++ {
		start := w * chunkSize
		end := start + chunkSize
		if end > len(vectors) {
			end = len(vectors)
		}
		wg.Add(1)
		go func(start, end int) {
			defer wg.Done()
			for i := start; i < end; i++ {
				bestC, minD := 0, float32(math.MaxFloat32)
				for c, cv := range centroids {
					if d := distSq(vectors[i], cv); d < minD {
						minD = d
						bestC = c
					}
				}
				assignments[i] = bestC
			}
		}(start, end)
	}
	wg.Wait()
	return assignments
}

// updateCentroids recalcula cada centróide como a média dos vetores do seu cluster.
func updateCentroids(vectors [][16]float32, assignments []int) [][16]float32 {
	sums := make([][16]float64, NumClusters)
	counts := make([]int, NumClusters)

	for i, c := range assignments {
		counts[c]++
		for d := 0; d < 14; d++ {
			sums[c][d] += float64(vectors[i][d])
		}
	}

	centroids := make([][16]float32, NumClusters)
	for c := 0; c < NumClusters; c++ {
		if counts[c] == 0 {
			// Centróide vazio: reinicializa com vetor aleatório
			centroids[c] = vectors[rand.Intn(len(vectors))]
			continue
		}
		for d := 0; d < 14; d++ {
			centroids[c][d] = float32(sums[c][d] / float64(counts[c]))
		}
	}
	return centroids
}

func distSq(a, b [16]float32) float32 {
	var s float32
	for i := 0; i < 14; i++ {
		d := a[i] - b[i]
		s += d * d
	}
	return s
}

// float32ToFloat16 converte um float32 para o formato IEEE 754 float16 (uint16).
func float32ToFloat16(f float32) uint16 {
	bits := math.Float32bits(f)
	sign := uint16((bits >> 31) & 0x1)
	exp := int32((bits>>23)&0xFF) - 127 + 15
	mantissa := bits & 0x7FFFFF

	if exp <= 0 {
		return sign << 15
	}
	if exp >= 31 {
		return (sign << 15) | (0x1F << 10)
	}
	return (sign << 15) | (uint16(exp) << 10) | uint16(mantissa>>13)
}

func saveOptimizedBinary(centroids [][16]float32, buckets [][]int, allVectors [][16]float32, labels []uint8) {
	f, err := os.Create("./resources/dataset_otimizado.bin")
	if err != nil {
		panic("Erro ao criar dataset_otimizado.bin: " + err.Error())
	}
	defer f.Close()

	w := bufio.NewWriterSize(f, 8*1024*1024) // buffer de 8MB

	// 1. Centróides: 1024 × 16 float32 (mantidos em f32 — são pequenos e usados internamente)
	binary.Write(w, binary.LittleEndian, centroids)

	// 2. Offsets dos buckets: 1025 uint32
	offsets := make([]uint32, NumClusters+1)
	curr := uint32(0)
	for i, b := range buckets {
		offsets[i] = curr
		curr += uint32(len(b))
	}
	offsets[NumClusters] = curr
	binary.Write(w, binary.LittleEndian, offsets)

	// 3. Vetores reordenados por bucket: N × 16 float16 (uint16) — metade do tamanho vs float32
	f16buf := make([]uint16, 16)
	for _, b := range buckets {
		for _, idx := range b {
			for d := 0; d < 16; d++ {
				f16buf[d] = float32ToFloat16(allVectors[idx][d])
			}
			binary.Write(w, binary.LittleEndian, f16buf)
		}
	}

	// 4. Labels reordenados: N × uint8
	for _, b := range buckets {
		for _, idx := range b {
			w.WriteByte(labels[idx])
		}
	}

	if err := w.Flush(); err != nil {
		panic("Erro ao fazer flush do binário: " + err.Error())
	}
}
