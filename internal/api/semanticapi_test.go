package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/laurenwilcox/name/internal/semantic"
)

type flatEmb struct{}

func (flatEmb) Embed(_ context.Context, _ string, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{1, float32(len(texts[i]) % 7)}
	}
	return out, nil
}

func TestSemanticAPI(t *testing.T) {
	h, st := setup(t)
	if rec := call(h, "GET", "/api/semantic", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("без движка: %d", rec.Code)
	}
	h.Semantic = semantic.New(flatEmb{}, st)
	dir := gitProject(t)
	call(h, "POST", "/api/projects", fmt.Sprintf(`{"path":%q}`, dir))

	if rec := call(h, "POST", "/api/semantic/index", fmt.Sprintf(`{"project":%q}`, dir)); rec.Code != http.StatusBadRequest {
		t.Fatalf("индекс без модели: %d", rec.Code)
	}
	if rec := call(h, "PUT", "/api/semantic", `{"model":"nomodel"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("модель без провайдера: %d", rec.Code)
	}
	if rec := call(h, "PUT", "/api/semantic", `{"model":"ollama/nomic-embed-text"}`); rec.Code != http.StatusOK {
		t.Fatalf("модель: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "POST", "/api/semantic/index", fmt.Sprintf(`{"project":%q}`, dir)); rec.Code != http.StatusAccepted {
		t.Fatalf("индекс: %d %s", rec.Code, rec.Body)
	}
	var s semantic.Status
	for i := 0; i < 100; i++ {
		json.Unmarshal(call(h, "GET", "/api/semantic?project="+dir, "").Body.Bytes(), &s)
		if !s.Indexing {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if s.Indexing || s.Error != "" || s.Total == 0 || s.Done != s.Total || s.Model != "ollama/nomic-embed-text" {
		t.Fatalf("статус: %+v", s)
	}
	rec := call(h, "GET", "/api/semantic/search?project="+dir+"&q=Add", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"hits"`) {
		t.Fatalf("поиск: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "GET", "/api/semantic/search?project=/nope&q=x", ""); rec.Code == http.StatusOK {
		t.Fatal("чужой проект")
	}
}
