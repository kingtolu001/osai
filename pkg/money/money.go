package money

import (
	"fmt"
	"strconv"
)

// Amount represents a monetary value in minor units without floating point.
type Amount int64

// Currency is an ISO 4217 code, normalized uppercase.
type Currency string

func (c Currency) String() string { return string(c) }

// New constructs an Amount from a minor-unit integer value.
func New(value int64) Amount { return Amount(value) }

// Add returns the sum of two amounts.
func (a Amount) Add(b Amount) Amount { return a + b }

// Sub returns the difference of two amounts.
func (a Amount) Sub(b Amount) Amount { return a - b }

// Neg returns the negative of an amount.
func (a Amount) Neg() Amount { return -a }

// String formats the amount as an integer minor-unit string.
func (a Amount) String() string { return strconv.FormatInt(int64(a), 10) }

// Parse parses a minor-unit integer string.
func Parse(value string) (Amount, error) {
	v, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse amount: %w", err)
	}
	return Amount(v), nil
}
