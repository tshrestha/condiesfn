package main

import (
	"encoding/hex"
	"fmt"
	"log"
	"nawa-functions/internal"
)

func main() {
	key := []byte("key")
	data := []byte("dat")

	ciphertext, err := internal.Encrypt(key, data)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("ciphertext: %s\n", hex.EncodeToString(ciphertext))

	plaintext, err := internal.Decrypt(key, ciphertext)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("plaintext: %s\n", plaintext)
}
