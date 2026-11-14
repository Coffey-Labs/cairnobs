package server

// Signed ID tokens (JWS compact form) checked against the provider's
// published keys (JWKS), with the standard library only. Asymmetric
// algorithms only: "none" and the HMAC ones are refused, so a token can't
// be signed with a key everyone knows (the client secret, or nothing).

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"math/big"
	"strings"
)

// jwk is one key of a JWKS.
type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

type jwks struct {
	Keys []jwk `json:"keys"`
}

func b64url(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

// public turns the JWK into a key; nil for a kind obs-beacond doesn't use.
func (k jwk) public() (crypto.PublicKey, error) {
	switch k.Kty {
	case "RSA":
		n, err1 := b64url(k.N)
		e, err2 := b64url(k.E)
		if err1 != nil || err2 != nil || len(n) < 256 || len(e) == 0 || len(e) > 4 {
			return nil, errors.New("bad RSA key (2048 bits at least)")
		}
		return &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}, nil
	case "EC":
		var c elliptic.Curve
		switch k.Crv {
		case "P-256":
			c = elliptic.P256()
		case "P-384":
			c = elliptic.P384()
		case "P-521":
			c = elliptic.P521()
		default:
			return nil, fmt.Errorf("EC curve %q", k.Crv)
		}
		x, err1 := b64url(k.X)
		y, err2 := b64url(k.Y)
		size := (c.Params().BitSize + 7) / 8
		if err1 != nil || err2 != nil || len(x) != size || len(y) != size {
			return nil, errors.New("bad EC key")
		}
		// Parsing as an uncompressed point checks it's on the curve.
		pk, err := ecdsa.ParseUncompressedPublicKey(c, append(append([]byte{4}, x...), y...))
		if err != nil {
			return nil, fmt.Errorf("bad EC key: %w", err)
		}
		return pk, nil
	case "OKP":
		x, err := b64url(k.X)
		if k.Crv != "Ed25519" || err != nil || len(x) != ed25519.PublicKeySize {
			return nil, errors.New("bad OKP key (Ed25519 only)")
		}
		return ed25519.PublicKey(x), nil
	}
	return nil, nil
}

// algHash is the hash an algorithm signs.
func algHash(alg string) (crypto.Hash, func() hash.Hash) {
	if len(alg) < 3 {
		return 0, nil
	}
	switch alg[len(alg)-3:] {
	case "256":
		return crypto.SHA256, sha256.New
	case "384":
		return crypto.SHA384, sha512.New384
	case "512":
		return crypto.SHA512, sha512.New
	}
	return 0, nil
}

var jwsAlgs = map[string]string{ // alg → the key type it needs
	"RS256": "RSA", "RS384": "RSA", "RS512": "RSA",
	"PS256": "RSA", "PS384": "RSA", "PS512": "RSA",
	"ES256": "EC", "ES384": "EC", "ES512": "EC",
	"EdDSA": "OKP",
}

type jwsHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	Typ string `json:"typ"`
}

// parseJWS splits a compact JWS and decodes its header and payload. Nothing
// is trusted until verifyJWS says so.
func parseJWS(tok string) (h jwsHeader, payload, signed, sig []byte, err error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return h, nil, nil, nil, errors.New("not a signed JWT")
	}
	hb, err := b64url(parts[0])
	if err != nil || json.Unmarshal(hb, &h) != nil {
		return h, nil, nil, nil, errors.New("bad JWT header")
	}
	if payload, err = b64url(parts[1]); err != nil {
		return h, nil, nil, nil, errors.New("bad JWT payload")
	}
	if sig, err = b64url(parts[2]); err != nil {
		return h, nil, nil, nil, errors.New("bad JWT signature")
	}
	return h, payload, []byte(parts[0] + "." + parts[1]), sig, nil
}

// pickKey finds the key for a header: by kid, or the only key of the right
// type when the token names none.
func pickKey(set jwks, h jwsHeader) (crypto.PublicKey, error) {
	kty := jwsAlgs[h.Alg]
	var found []jwk
	for _, k := range set.Keys {
		if k.Kty != kty || (k.Use != "" && k.Use != "sig") || (k.Alg != "" && k.Alg != h.Alg) {
			continue
		}
		if h.Kid != "" && k.Kid != h.Kid {
			continue
		}
		found = append(found, k)
	}
	if len(found) != 1 {
		return nil, errNoKey
	}
	pk, err := found[0].public()
	if err == nil && pk == nil {
		err = errNoKey
	}
	return pk, err
}

var errNoKey = errors.New("no matching key in the provider's JWKS")

// verifyJWS checks the signature with key.
func verifyJWS(alg string, key crypto.PublicKey, signed, sig []byte) error {
	ch, newHash := algHash(alg)
	if newHash == nil && alg != "EdDSA" {
		return fmt.Errorf("algorithm %q", alg)
	}
	var digest []byte
	if newHash != nil {
		hh := newHash()
		hh.Write(signed)
		digest = hh.Sum(nil)
	}
	bad := errors.New("ID token signature doesn't verify")
	switch k := key.(type) {
	case *rsa.PublicKey:
		var err error
		switch alg[:2] {
		case "RS":
			err = rsa.VerifyPKCS1v15(k, ch, digest, sig)
		case "PS":
			err = rsa.VerifyPSS(k, ch, digest, sig, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
		default:
			return bad
		}
		if err != nil {
			return bad
		}
	case *ecdsa.PublicKey:
		size := (k.Curve.Params().BitSize + 7) / 8
		want := map[string]int{"ES256": 32, "ES384": 48, "ES512": 66}[alg]
		if alg[:2] != "ES" || size != want || len(sig) != 2*size {
			return bad
		}
		r, s := new(big.Int).SetBytes(sig[:size]), new(big.Int).SetBytes(sig[size:])
		if !ecdsa.Verify(k, digest, r, s) {
			return bad
		}
	case ed25519.PublicKey:
		if alg != "EdDSA" || !ed25519.Verify(k, signed, sig) {
			return bad
		}
	default:
		return bad
	}
	return nil
}
