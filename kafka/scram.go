package kafka

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"hash"

	"github.com/xdg-go/scram"
)

var (
	SHA256 scram.HashGeneratorFcn = sha256.New
	SHA512 scram.HashGeneratorFcn = sha512.New
)

// XDGSCRAMClient implements sarama.SCRAMClient using xdg-go/scram
type XDGSCRAMClient struct {
	*scram.Client
	*scram.ClientConversation
	scram.HashGeneratorFcn
}

// Begin starts the SCRAM authentication process
func (x *XDGSCRAMClient) Begin(userName, password, authzID string) (err error) {
	x.Client, err = x.HashGeneratorFcn.NewClient(userName, password, authzID)
	if err != nil {
		return err
	}
	x.ClientConversation = x.Client.NewConversation()
	return nil
}

// Step performs a step in the SCRAM authentication
func (x *XDGSCRAMClient) Step(challenge string) (response string, err error) {
	response, err = x.ClientConversation.Step(challenge)
	return
}

// Done checks if SCRAM authentication is complete
func (x *XDGSCRAMClient) Done() bool {
	return x.ClientConversation.Done()
}

// SHA256Hash generates a SHA256 hash
type SHA256Hash struct {
	hash.Hash
}

// New creates a new SHA256 hash
func (s *SHA256Hash) New() hash.Hash {
	return sha256.New()
}

// HMAC creates a new HMAC-SHA256
func (s *SHA256Hash) HMAC(key []byte) hash.Hash {
	return hmac.New(sha256.New, key)
}

// SHA512Hash generates a SHA512 hash
type SHA512Hash struct {
	hash.Hash
}

// New creates a new SHA512 hash
func (s *SHA512Hash) New() hash.Hash {
	return sha512.New()
}

// HMAC creates a new HMAC-SHA512
func (s *SHA512Hash) HMAC(key []byte) hash.Hash {
	return hmac.New(sha512.New, key)
}
