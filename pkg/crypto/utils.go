package crypto

import "crypto/rand"

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
