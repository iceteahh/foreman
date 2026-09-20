// Command api serves the checkout endpoints.
package main

import (
	"fmt"
	"net/http"

	"checkout-api/internal/cache"
)

var prices = cache.New()

func main() {
	http.HandleFunc("/price", handlePrice)
	fmt.Println("listening on :8080")
	_ = http.ListenAndServe(":8080", nil) //nolint:gosec // demo fixture
}

// handlePrice answers a price lookup from the shared cache.
func handlePrice(w http.ResponseWriter, r *http.Request) {
	sku := r.URL.Query().Get("sku")
	if v, ok := prices.Get(sku); ok {
		fmt.Fprintf(w, "%d", v)
		return
	}
	price := lookupPrice(sku)
	prices.Set(sku, price)
	fmt.Fprintf(w, "%d", price)
}

func lookupPrice(sku string) int64 {
	return int64(len(sku)) * 100
}
