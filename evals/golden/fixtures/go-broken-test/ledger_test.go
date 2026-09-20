package ledger

import (
	"errors"
	"testing"
)

func TestDeposit(t *testing.T) {
	var a Account
	if err := a.Deposit(250); err != nil || a.Cents != 250 {
		t.Fatalf("Deposit: %v, balance %d", err, a.Cents)
	}
}

func TestWithdrawRefusesOverdraft(t *testing.T) {
	a := Account{Cents: 100}
	err := a.Withdraw(500)
	if !errors.Is(err, ErrInsufficient) {
		t.Fatalf("Withdraw(500) on a balance of 100 = %v, want ErrInsufficient", err)
	}
	if a.Cents != 100 {
		t.Fatalf("a refused withdrawal changed the balance to %d, want 100", a.Cents)
	}
}
