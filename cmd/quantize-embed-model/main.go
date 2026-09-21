// Command quantize-embed-model rewrites a float32 model2vec safetensors file with
// its embedding matrix stored as int8 plus one scale per row. It exists so the
// committed model blob under internal/embed/models has reproducible provenance:
//
//	go run ./cmd/quantize-embed-model in.safetensors out.safetensors
//
// Quantization is retrieval-neutral for a static embedder (the mean over a chunk's
// tokens cancels the per-element error, and L2 normalization discards the
// residual scale), and it is why the upstream project distributes these models at
// roughly one byte per parameter rather than four.
package main

import (
	"fmt"
	"os"

	"github.com/neur0map/prowl/internal/embed"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: quantize-embed-model <in.safetensors> <out.safetensors>")
		os.Exit(2)
	}
	in, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "read:", err)
		os.Exit(1)
	}
	out, err := embed.QuantizeStatic(in)
	if err != nil {
		fmt.Fprintln(os.Stderr, "quantize:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(os.Args[2], out, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
		os.Exit(1)
	}
	fmt.Printf("%s: %.1f MB -> %s: %.1f MB\n",
		os.Args[1], float64(len(in))/1048576, os.Args[2], float64(len(out))/1048576)
}
