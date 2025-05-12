package crypto

import (
	"crypto/rand"
	"fmt"
)

func RandomBytes(size int) ([]byte, error) {
	b := make([]byte, size)
	_, err := rand.Read(b)
	if err != nil {
		return nil, err
	}

	return b, nil
}

func GenerateSeed() []byte {
	res, _ := RandomBytes(64)
	return res
}

// TODO: Remove it?
func safeInt(u uint64) (int, error) {
	if u > uint64(^uint(0)>>1) {
		return 0, fmt.Errorf("value too large to fit into int: %d", u)
	}
	return int(u), nil
}
