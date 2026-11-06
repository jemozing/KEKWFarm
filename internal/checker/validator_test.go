package checker

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestCapsuleValidator(t *testing.T) {
	es, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rs, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		alg string
		key crypto.Signer
	}{{"ES256", es}, {"RS256", rs}} {
		t.Run(test.alg, func(t *testing.T) {
			der, err := x509.MarshalPKIXPublicKey(test.key.Public())
			if err != nil {
				t.Fatal(err)
			}
			publicKey := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
			validate, err := IngestValidator(Config{Options: map[string]string{"validator": "volgactf", "public_key": publicKey}})
			if err != nil {
				t.Fatal(err)
			}
			const flag = "0123456789abcdef0123456789abcdef="
			token := makeCapsule(t, test.key, test.alg, map[string]any{"flag": flag})
			for _, capsule := range []string{token, "VolgaCTF{" + token + "}"} {
				if got, err := validate(capsule); err != nil || got != flag {
					t.Fatalf("validate valid capsule got %q, %v", got, err)
				}
			}
			parts := strings.Split(token, ".")
			parts[1] = base64.RawURLEncoding.EncodeToString([]byte(`{"flag":"changed"}`))
			for _, invalid := range []string{
				strings.Join(parts, "."),
				makeCapsule(t, test.key, "none", map[string]any{"flag": flag}),
				makeCapsule(t, test.key, test.alg, map[string]any{"flag": flag, "exp": time.Now().Add(-time.Minute).Unix()}),
				makeCapsule(t, test.key, test.alg, map[string]any{"flag": flag, "nbf": time.Now().Add(time.Minute).Unix()}),
			} {
				if _, err := validate(invalid); err == nil {
					t.Fatal("validator accepted an invalid capsule")
				}
			}
		})
	}
}

func makeCapsule(t *testing.T, key crypto.Signer, alg string, claims map[string]any) string {
	t.Helper()
	encoding := base64.RawURLEncoding
	header := encoding.EncodeToString(fmt.Appendf(nil, `{"alg":%q}`, alg))
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	message := header + "." + encoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(message))
	var signature []byte
	if es, ok := key.(*ecdsa.PrivateKey); ok {
		r, s, err := ecdsa.Sign(rand.Reader, es, digest[:])
		if err != nil {
			t.Fatal(err)
		}
		signature = make([]byte, 64)
		r.FillBytes(signature[:32])
		s.FillBytes(signature[32:])
	} else {
		signature, err = key.Sign(rand.Reader, digest[:], crypto.SHA256)
		if err != nil {
			t.Fatal(err)
		}
	}
	return message + "." + encoding.EncodeToString(signature)
}
