package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// LM Studio lowercases a model's folder name, so an ID with capitals would
// never be found; the config refuses it, naming the ID it would get.
func TestLmStudio_RejectsAnUppercaseID(t *testing.T) {
	_, err := writeConfig(t, "  lmStudio: {models: [{id: Model, file: a.gguf}]}\n")
	wantError(t, err, "'model'")
}

// LM Studio's model list answers 500 while a model loads; the error says
// that is why, and keeps LM Studio's own answer.
func TestLmStudio_ModelsDuringALoadSaysWhy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error": {"message": "Model does not exist."}}`, http.StatusInternalServerError)
	}))
	defer srv.Close()
	_, err := (&lmStudio{}).models(context.Background(), backend{Name: "lmStudio", URL: srv.URL})
	wantError(t, err, "while it loads a model")
	wantError(t, err, "Model does not exist.")
}
