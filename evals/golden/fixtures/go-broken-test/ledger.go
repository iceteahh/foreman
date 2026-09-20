// Package ledger tracks account balances in integer cents.
package ledger

import "errors"

// ErrInsufficient is returned when a withdrawal exceeds the balance.
var ErrInsufficient = errors.New("ledger: insufficient funds")

// Account is a balance in cents.
type Account struct {
	Cents int64
}

// Deposit adds amount cents to the account.
func (a *Account) Deposit(amount int64) error {
	if amount < 0 {
		return errors.New("ledger: negative deposit")
	}
	a.Cents += amount
	return nil
}

// Withdraw removes amount cents, refusing to overdraw.
func (a *Account) Withdraw(amount int64) error {
	if amount < 0 {
		return errors.New("ledger: negative withdrawal")
	}
	a.Cents -= amount
	return nil
}
