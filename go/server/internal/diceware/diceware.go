// Package diceware makes the passphrases agents log in with (signup and
// passphrase rotation).
package diceware

import (
	"crypto/rand"
	_ "embed"
	"math/big"
	"strings"
)

// eff_large_wordlist.txt is the EFF large Diceware word list (7776 words,
// CC BY 3.0, https://www.eff.org/dice), as the eff-diceware-passphrase
// package Node used ships it.
//
//go:embed eff_large_wordlist.txt
var wordlistText string

var wordlist = strings.Fields(wordlistText)

// Passphrase is three distinct words in random order joined with '-', as
// generatePassphrase(3).join('-'): the package samples three distinct
// indexes and shuffles them, which is a uniform draw of an ordered triple of
// distinct words.
func Passphrase() string {
	picked := map[int]bool{}
	words := make([]string, 0, 3)
	for len(words) < 3 {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(wordlist))))
		if err != nil {
			panic(err)
		}
		i := int(n.Int64())
		if picked[i] {
			continue
		}
		picked[i] = true
		words = append(words, wordlist[i])
	}
	return strings.Join(words, "-")
}
