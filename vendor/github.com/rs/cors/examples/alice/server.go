package main

import (
	"net/http"

	"github.com/justinas/alice"
	"github.com/rs/cors"
)

func main() {
	c := cors.New(cors.Options{
		AllowedOrigins: []string{"http://foo.com"},
	})

	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("{\"hello\": \"world\"}"))
	})

	chain := alice.New(c.Handler).Then(mux)
	http.ListenAndServe(":8080", chain)
}
