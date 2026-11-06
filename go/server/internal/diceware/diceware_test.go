package diceware

import (
	"strings"
	"testing"
)

func TestPassphrase(t *testing.T) {
	if len(wordlist) != 7776 || wordlist[0] != "abacus" {
		t.Fatalf("wordlist %d", len(wordlist))
	}
	words := strings.Split(Passphrase(), "-")
	if len(words) < 3 {
		t.Fatalf("%v", words)
	}
}
