// ABOUTME: Utility to generate a KeibiDrop keypair and print the fingerprint for attacker_traversal.go.
// ABOUTME: Run standalone with: go run gen_attacker.go

//go:build ignore

package main
import (
	"fmt"
	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
)
func main() {
	kemDec, kemEnc, _ := kbc.GenerateMLKEMKeypair()
	xPriv, xPub, _ := kbc.GenerateX25519Keypair()
	ok := kbc.OwnKeys{
		MlKemPrivate: kemDec,
		MlKemPublic:  kemEnc,
		X25519Private: xPriv,
		X25519Public:  xPub,
	}
	fp, _ := ok.Fingerprint()
	fmt.Println(fp)
}
