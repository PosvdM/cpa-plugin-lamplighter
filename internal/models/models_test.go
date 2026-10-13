package models

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestListerSendsAPIKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer k" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"data":[{"id":"gpt-6-luna","owned_by":"openai"}]}`))
	}))
	defer server.Close()
	got, err := (&Lister{}).List(context.Background(), server.URL, "k")
	if err != nil || !reflect.DeepEqual(got, []string{"gpt-6-luna"}) {
		t.Fatalf("got %v %v", got, err)
	}
	if _, err := (&Lister{}).List(context.Background(), server.URL, ""); err == nil {
		t.Fatal("missing key must fail")
	}
}
