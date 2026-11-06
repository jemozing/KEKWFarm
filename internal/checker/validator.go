package checker

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// IngestValidator returns a normalizer for flags received by the farm. A nil
// normalizer means the caller can store the flag as received.
func IngestValidator(config Config) (func(string) (string, error), error) {
	switch config.Options["validator"] {
	case "":
		if config.Options["public_key"] != "" {
			return nil, errors.New("checker public_key requires a validator")
		}
		return nil, nil
	case "volgactf":
		return capsuleValidator(config.Options["public_key"])
	default:
		return nil, fmt.Errorf("unknown checker validator %q", config.Options["validator"])
	}
}

func capsuleValidator(keyPEM string) (func(string) (string, error), error) {
	block, rest := pem.Decode([]byte(keyPEM))
	if block == nil || len(strings.TrimSpace(string(rest))) > 0 {
		return nil, errors.New("VolgaCTF validator requires a single PEM public_key")
	}
	var publicKey any
	var err error
	switch block.Type {
	case "PUBLIC KEY":
		publicKey, err = x509.ParsePKIXPublicKey(block.Bytes)
	case "RSA PUBLIC KEY":
		publicKey, err = x509.ParsePKCS1PublicKey(block.Bytes)
	default:
		return nil, errors.New("VolgaCTF validator requires an ES256 or RS256 public key")
	}
	if err != nil {
		return nil, fmt.Errorf("invalid VolgaCTF validator public key: %w", err)
	}
	verify, err := capsuleSignatureVerifier(publicKey)
	if err != nil {
		return nil, err
	}
	return func(capsule string) (string, error) {
		return decodeCapsule(capsule, verify)
	}, nil
}

func capsuleSignatureVerifier(publicKey any) (func(string, []byte, []byte) bool, error) {
	switch key := publicKey.(type) {
	case *ecdsa.PublicKey:
		if key.Curve != elliptic.P256() {
			return nil, errors.New("VolgaCTF ES256 key must use P-256")
		}
		return func(alg string, digest, signature []byte) bool {
			if alg != "ES256" || len(signature) != 64 {
				return false
			}
			r := new(big.Int).SetBytes(signature[:32])
			s := new(big.Int).SetBytes(signature[32:])
			return ecdsa.Verify(key, digest, r, s)
		}, nil
	case *rsa.PublicKey:
		return func(alg string, digest, signature []byte) bool {
			return alg == "RS256" && rsa.VerifyPKCS1v15(key, crypto.SHA256, digest, signature) == nil
		}, nil
	default:
		return nil, errors.New("VolgaCTF validator requires an ES256 or RS256 public key")
	}
}

func decodeCapsule(capsule string, verify func(string, []byte, []byte) bool) (string, error) {
	if unwrapped, ok := strings.CutPrefix(capsule, "VolgaCTF{"); ok {
		if !strings.HasSuffix(unwrapped, "}") {
			return "", errors.New("invalid VolgaCTF capsule wrapper")
		}
		capsule = strings.TrimSuffix(unwrapped, "}")
	}
	parts := strings.Split(capsule, ".")
	if len(parts) != 3 {
		return "", errors.New("invalid VolgaCTF capsule format")
	}
	encoding := base64.RawURLEncoding.Strict()
	headerData, err := encoding.DecodeString(parts[0])
	if err != nil {
		return "", errors.New("invalid VolgaCTF capsule header")
	}
	var header struct {
		Algorithm string          `json:"alg"`
		Critical  []string        `json:"crit"`
		B64       json.RawMessage `json:"b64"`
	}
	if err := json.Unmarshal(headerData, &header); err != nil || len(header.Critical) != 0 || len(header.B64) != 0 {
		return "", errors.New("invalid VolgaCTF capsule header")
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	signature, err := encoding.DecodeString(parts[2])
	if err != nil || !verify(header.Algorithm, digest[:], signature) {
		return "", errors.New("invalid VolgaCTF capsule signature")
	}
	payload, err := encoding.DecodeString(parts[1])
	if err != nil {
		return "", errors.New("invalid VolgaCTF capsule payload")
	}
	var claims struct {
		Flag      string          `json:"flag"`
		Expires   json.Number     `json:"exp"`
		NotBefore json.Number     `json:"nbf"`
		IssuedAt  json.Number     `json:"iat"`
		Audience  json.RawMessage `json:"aud"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Flag == "" || strings.ContainsAny(claims.Flag, "\r\n") {
		return "", errors.New("invalid VolgaCTF capsule flag")
	}
	if len(claims.Audience) > 0 && string(claims.Audience) != `null` && string(claims.Audience) != `""` && string(claims.Audience) != `[]` {
		return "", errors.New("VolgaCTF capsule requires an audience")
	}
	now := float64(time.Now().UnixNano()) / float64(time.Second)
	if err := checkCapsuleTime(claims.Expires, now, true); err != nil {
		return "", err
	}
	if err := checkCapsuleTime(claims.NotBefore, now, false); err != nil {
		return "", err
	}
	if err := checkCapsuleTime(claims.IssuedAt, now, false); err != nil {
		return "", err
	}
	return claims.Flag, nil
}

func checkCapsuleTime(claim json.Number, now float64, expires bool) error {
	if claim == "" {
		return nil
	}
	value, err := claim.Float64()
	if err != nil {
		return errors.New("invalid VolgaCTF capsule timestamp")
	}
	if expires && value <= now {
		return errors.New("expired VolgaCTF capsule")
	}
	if !expires && value > now {
		return errors.New("VolgaCTF capsule is not valid yet")
	}
	return nil
}
