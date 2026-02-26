package mock

import (
	"context"
	"errors"
)

// IndexdExplorer is a mock implementation of the explorer interfaces
// used by indexd's pins and admin packages.
type IndexdExplorer struct {
	rates map[string]float64
}

// NewIndexdExplorer creates a new mock explorer with a default USD exchange
// rate of 1.0.
func NewIndexdExplorer() *IndexdExplorer {
	return &IndexdExplorer{
		rates: map[string]float64{
			"usd": 1.0,
		},
	}
}

// BaseURL returns the base URL of the explorer.
func (e *IndexdExplorer) BaseURL() string {
	return "https://explorer.internal"
}

// SiacoinExchangeRate returns the exchange rate for a given currency.
func (e *IndexdExplorer) SiacoinExchangeRate(_ context.Context, currency string) (float64, error) {
	if rate, ok := e.rates[currency]; ok {
		return rate, nil
	}
	return 0, errors.New("currency not supported")
}
