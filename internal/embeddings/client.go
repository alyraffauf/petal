package embeddings

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
)

var ErrUnavailable = errors.New("embedding provider unavailable")

func embeddingRequest(ctx context.Context, cfg Settings, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, cfg.BaseURL+path, body)
	if err != nil {
		return nil, err
	}
	if cfg.APIKeyEnv != "" {
		key := os.Getenv(cfg.APIKeyEnv)
		if key == "" {
			return nil, ErrUnavailable
		}
		req.Header.Set("Authorization", "Bearer "+key)
	}
	return req, nil
}

func (cfg Settings) CacheKey(ctx context.Context, client *http.Client) (string, error) {
	identity := cfg.Provider + "\x00" + cfg.BaseURL + "\x00" + cfg.Model
	if cfg.Provider == "openai-compatible" {
		return fmt.Sprintf("%x", sha256.Sum256([]byte(identity))), nil
	}
	req, err := embeddingRequest(ctx, cfg, "GET", "/api/tags", nil)
	if err != nil {
		return "", err
	}
	res, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	var tags struct {
		Models []struct{ Name, Digest string }
	}
	if res.StatusCode == 200 && json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&tags) == nil {
		for _, m := range tags.Models {
			if m.Name == cfg.Model && len(m.Digest) == 64 {
				if _, err := hex.DecodeString(m.Digest); err == nil {
					return fmt.Sprintf("%x", sha256.Sum256([]byte(identity+"\x00"+m.Digest))), nil
				}
			}
		}
	}
	return "", ErrUnavailable
}

func (cfg Settings) Embed(ctx context.Context, client *http.Client, inputs []string) ([][]float32, string, error) {
	body, err := json.Marshal(map[string]any{"model": cfg.Model, "input": inputs})
	if err != nil {
		return nil, "", err
	}
	path := "/api/embed"
	if cfg.Provider == "openai-compatible" {
		path = "/embeddings"
	}
	req, err := embeddingRequest(ctx, cfg, "POST", path, bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, "", ErrUnavailable
	}
	var reply struct {
		Model      string
		Embeddings [][]float32
		Data       []struct {
			Index     *int
			Embedding []float32
		}
	}
	if err = json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&reply); err != nil {
		return nil, "", err
	}
	if reply.Model == "" || (cfg.Provider == "ollama" && reply.Model != cfg.Model) {
		return nil, "", ErrUnavailable
	}
	if cfg.Provider == "openai-compatible" {
		if len(reply.Data) != len(inputs) {
			return nil, "", ErrUnavailable
		}
		reply.Embeddings = make([][]float32, len(inputs))
		for _, d := range reply.Data {
			if len(d.Embedding) == 0 || d.Index == nil || *d.Index < 0 || *d.Index >= len(inputs) || reply.Embeddings[*d.Index] != nil {
				return nil, "", ErrUnavailable
			}
			reply.Embeddings[*d.Index] = d.Embedding
		}
	}
	if len(reply.Embeddings) != len(inputs) {
		return nil, "", errors.New("incomplete embeddings")
	}
	for _, v := range reply.Embeddings {
		if len(v) == 0 || len(v) != len(reply.Embeddings[0]) {
			return nil, "", errors.New("unexpected embedding dimensions")
		}
		var norm float64
		for _, x := range v {
			norm += float64(x) * float64(x)
		}
		if norm == 0 {
			return nil, "", ErrUnavailable
		}
		norm = math.Sqrt(norm)
		for i := range v {
			v[i] = float32(float64(v[i]) / norm)
		}
	}
	return reply.Embeddings, reply.Model, nil
}
