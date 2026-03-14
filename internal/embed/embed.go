package embed

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/knights-analytics/hugot"
	"github.com/knights-analytics/hugot/pipelines"
)

const (
	modelName    = "Snowflake/snowflake-arctic-embed-xs"
	pipelineName = "prowl-embed"
	embeddingDim = 384
	batchSize    = 32
)

// Embedder wraps a hugot FeatureExtractionPipeline for generating embeddings.
type Embedder struct {
	session  *hugot.Session
	pipeline *pipelines.FeatureExtractionPipeline
}

// New loads the embedding model. Downloads from HuggingFace if not cached.
// modelDir is typically ~/.prowl/models/
func New(modelDir string) (*Embedder, error) {
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		return nil, fmt.Errorf("embed: create model dir: %w", err)
	}

	// Check if model is already downloaded by looking for the expected directory.
	modelPath := filepath.Join(modelDir, "Snowflake_snowflake-arctic-embed-xs")
	if _, err := os.Stat(modelPath); os.IsNotExist(err) {
		var dlErr error
		modelPath, dlErr = hugot.DownloadModel(modelName, modelDir, hugot.NewDownloadOptions())
		if dlErr != nil {
			return nil, fmt.Errorf("embed: download model: %w", dlErr)
		}
	}

	session, err := hugot.NewGoSession()
	if err != nil {
		return nil, fmt.Errorf("embed: create session: %w", err)
	}

	config := hugot.FeatureExtractionConfig{
		ModelPath: modelPath,
		Name:      pipelineName,
		Options: []hugot.FeatureExtractionOption{
			pipelines.WithNormalization(),
		},
	}
	pipeline, err := hugot.NewPipeline(session, config)
	if err != nil {
		session.Destroy()
		return nil, fmt.Errorf("embed: create pipeline: %w", err)
	}

	return &Embedder{
		session:  session,
		pipeline: pipeline,
	}, nil
}

// Encode converts text strings into normalized embedding vectors.
// Texts are batched internally (32 at a time).
func (e *Embedder) Encode(texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	result := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += batchSize {
		end := start + batchSize
		if end > len(texts) {
			end = len(texts)
		}
		batch := texts[start:end]

		output, err := e.pipeline.RunPipeline(batch)
		if err != nil {
			return nil, fmt.Errorf("embed: encode batch [%d:%d]: %w", start, end, err)
		}
		result = append(result, output.Embeddings...)
	}
	return result, nil
}

// Dim returns the embedding dimension.
func (e *Embedder) Dim() int {
	return embeddingDim
}

// Close releases resources.
func (e *Embedder) Close() {
	if e.session != nil {
		e.session.Destroy()
		e.session = nil
	}
}
