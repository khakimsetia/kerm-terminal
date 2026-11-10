package main

import (
	"bytes"
	"crypto/aes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
	"testing"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/ssh"
)

// Test-only PPK encoder (independent implementation of the PuTTY spec),
// used to verify the decoder in ppk.go end-to-end.

func encStr(b []byte) []byte {
	out := make([]byte, 4)
	binary.BigEndian.PutUint32(out, uint32(len(b)))
	return append(out, b...)
}

func encMpint(b []byte) []byte {
	for len(b) > 1 && b[0] == 0 {
		b = b[1:]
	}
	if len(b) > 0 && b[0]&0x80 != 0 {
		b = append([]byte{0}, b...)
	}
	return encStr(b)
}

func encMpintBig(n *big.Int) []byte { return encMpint(n.Bytes()) }

func buildPPK(t *testing.T, version int, alg, comment string, pub, priv []byte, pass string) string {
	t.Helper()
	encryption := "none"
	var privEnc []byte
	var macKey []byte
	if pass != "" {
		encryption = "aes256-cbc"
		// pad priv to 16 with random bytes
		pad := (aes.BlockSize - (len(priv) % aes.BlockSize)) % aes.BlockSize
		padded := append(append([]byte{}, priv...), make([]byte, pad)...)
		if _, err := rand.Read(padded[len(priv):]); err != nil {
			t.Fatal(err)
		}
		var key, iv []byte
		if version == 2 {
			h1 := sha1.Sum(append([]byte{0, 0, 0, 0}, []byte(pass)...))
			h2 := sha1.Sum(append([]byte{0, 0, 0, 1}, []byte(pass)...))
			key = append(h1[:], h2[:]...)[:32]
			iv = make([]byte, 16)
			m := sha1.Sum(append([]byte("putty-private-key-file-mac-key"), []byte(pass)...))
			macKey = m[:]
		} else {
			salt := make([]byte, 16)
			if _, err := rand.Read(salt); err != nil {
				t.Fatal(err)
			}
			derived := argon2.IDKey([]byte(pass), salt, 2, 1024, 1, 80)
			key, iv, macKey = derived[:32], derived[32:48], derived[48:80]
			// store salt for the decoder via headers below (we rebuild with salt)
			return buildPPKWithSalt(t, version, alg, comment, pub, priv, pass, salt, key, iv, macKey)
		}
		block, _ := aes.NewCipher(key)
		privEnc = make([]byte, len(padded))
		prev := iv
		for off := 0; off < len(padded); off += aes.BlockSize {
			tmp := make([]byte, aes.BlockSize)
			copy(tmp, padded[off:off+aes.BlockSize])
			for j := range tmp {
				tmp[j] ^= prev[j]
			}
			block.Encrypt(privEnc[off:], tmp)
			prev = privEnc[off : off+aes.BlockSize]
		}
		priv = padded
	} else {
		privEnc = priv
		if version == 2 {
			m := sha1.Sum([]byte("putty-private-key-file-mac-key"))
			macKey = m[:]
		} else {
			macKey = []byte{}
		}
	}
	macPre := bytes.Join([][]byte{encStr([]byte(alg)), encStr([]byte(encryption)), encStr([]byte(comment)), encStr(pub), encStr(priv)}, nil)
	var mac []byte
	if version == 2 {
		h := hmac.New(sha1.New, macKey)
		h.Write(macPre)
		mac = h.Sum(nil)
	} else {
		h := hmac.New(sha256.New, macKey)
		h.Write(macPre)
		mac = h.Sum(nil)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "PuTTY-User-Key-File-%d: %s\n", version, alg)
	fmt.Fprintf(&sb, "Encryption: %s\n", encryption)
	fmt.Fprintf(&sb, "Comment: %s\n", comment)
	writeLines := func(label string, data []byte) {
		b64 := base64.StdEncoding.EncodeToString(data)
		n := (len(b64) + 63) / 64
		fmt.Fprintf(&sb, "%s: %d\n", label, n)
		for i := 0; i < len(b64); i += 64 {
			end := i + 64
			if end > len(b64) {
				end = len(b64)
			}
			sb.WriteString(b64[i:end] + "\n")
		}
	}
	writeLines("Public-Lines", pub)
	writeLines("Private-Lines", privEnc)
	fmt.Fprintf(&sb, "Private-MAC: %s\n", hex.EncodeToString(mac))
	return sb.String()
}

func buildPPKWithSalt(t *testing.T, version int, alg, comment string, pub, priv []byte, pass string, salt, key, iv, macKey []byte) string {
	t.Helper()
	pad := (aes.BlockSize - (len(priv) % aes.BlockSize)) % aes.BlockSize
	padded := append(append([]byte{}, priv...), make([]byte, pad)...)
	if _, err := rand.Read(padded[len(priv):]); err != nil {
		t.Fatal(err)
	}
	block, _ := aes.NewCipher(key)
	privEnc := make([]byte, len(padded))
	prev := iv
	for off := 0; off < len(padded); off += aes.BlockSize {
		tmp := make([]byte, aes.BlockSize)
		copy(tmp, padded[off:off+aes.BlockSize])
		for j := range tmp {
			tmp[j] ^= prev[j]
		}
		block.Encrypt(privEnc[off:], tmp)
		prev = privEnc[off : off+aes.BlockSize]
	}
	macPre := bytes.Join([][]byte{encStr([]byte(alg)), encStr([]byte("aes256-cbc")), encStr([]byte(comment)), encStr(pub), encStr(padded)}, nil)
	h := hmac.New(sha256.New, macKey)
	h.Write(macPre)
	mac := h.Sum(nil)
	var sb strings.Builder
	fmt.Fprintf(&sb, "PuTTY-User-Key-File-%d: %s\n", version, alg)
	sb.WriteString("Encryption: aes256-cbc\n")
	fmt.Fprintf(&sb, "Comment: %s\n", comment)
	sb.WriteString("Key-Derivation: Argon2id\n")
	fmt.Fprintf(&sb, "Argon2-Memory: %d\n", 1024)
	fmt.Fprintf(&sb, "Argon2-Passes: %d\n", 2)
	fmt.Fprintf(&sb, "Argon2-Parallelism: %d\n", 1)
	fmt.Fprintf(&sb, "Argon2-Salt: %s\n", hex.EncodeToString(salt))
	writeLines := func(label string, data []byte) {
		b64 := base64.StdEncoding.EncodeToString(data)
		n := (len(b64) + 63) / 64
		fmt.Fprintf(&sb, "%s: %d\n", label, n)
		for i := 0; i < len(b64); i += 64 {
			end := i + 64
			if end > len(b64) {
				end = len(b64)
			}
			sb.WriteString(b64[i:end] + "\n")
		}
	}
	writeLines("Public-Lines", pub)
	writeLines("Private-Lines", privEnc)
	fmt.Fprintf(&sb, "Private-MAC: %s\n", hex.EncodeToString(mac))
	return sb.String()
}

func checkPPK(t *testing.T, ppk, pass string, expectPub []byte) {
	t.Helper()
	pem, _, err := PPKToOpenSSH(ppk, pass)
	if err != nil {
		t.Fatalf("PPKToOpenSSH: %v", err)
	}
	signer, err := ssh.ParsePrivateKey(pem)
	if err != nil {
		t.Fatalf("ParsePrivateKey(converted): %v", err)
	}
	got := signer.PublicKey().Marshal()
	if !bytes.Equal(got, expectPub) {
		t.Fatalf("public mismatch:\n got %x\nwant %x", got, expectPub)
	}
}

func TestPPKRSA(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pub := bytes.Join([][]byte{encStr([]byte("ssh-rsa")), encMpintBig(big.NewInt(int64(key.E))), encMpintBig(key.N)}, nil)
	iqmp := new(big.Int).ModInverse(key.Primes[1], key.Primes[0])
	priv := bytes.Join([][]byte{encMpintBig(key.D), encMpintBig(key.Primes[0]), encMpintBig(key.Primes[1]), encMpintBig(iqmp)}, nil)
	// v2 plain
	checkPPK(t, buildPPK(t, 2, "ssh-rsa", "test", pub, priv, ""), "", pub)
	// v2 encrypted + wrong passphrase
	ppkEnc := buildPPK(t, 2, "ssh-rsa", "test", pub, priv, "test123")
	checkPPK(t, ppkEnc, "test123", pub)
	if _, _, err := PPKToOpenSSH(ppkEnc, "wrong"); err == nil || !strings.Contains(err.Error(), "MAC") {
		t.Fatalf("wrong passphrase should fail MAC, got %v", err)
	}
	// v3 encrypted Argon2id
	checkPPK(t, buildPPK(t, 3, "ssh-rsa", "test", pub, priv, "test123"), "test123", pub)
}

func TestPPKECDSAEd25519(t *testing.T) {
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	point := elliptic.Marshal(elliptic.P256(), ec.PublicKey.X, ec.PublicKey.Y)
	pub := bytes.Join([][]byte{encStr([]byte("ecdsa-sha2-nistp256")), encStr([]byte("nistp256")), encStr(point)}, nil)
	priv := encMpintBig(ec.D)
	checkPPK(t, buildPPK(t, 3, "ecdsa-sha2-nistp256", "test", pub, priv, "test123"), "test123", pub)

	pubK, seed, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	edPub := bytes.Join([][]byte{encStr([]byte("ssh-ed25519")), encStr(pubK)}, nil)
	edPriv := encStr(append(append([]byte{}, seed.Seed()...), pubK...))
	checkPPK(t, buildPPK(t, 3, "ssh-ed25519", "test", edPub, edPriv, ""), "", edPub)
	checkPPK(t, buildPPK(t, 3, "ssh-ed25519", "test", edPub, edPriv, "test123"), "test123", edPub)
}

func TestPPKNotPPK(t *testing.T) {
	if _, _, err := PPKToOpenSSH("not a ppk", ""); err == nil {
		t.Fatalf("expected error for non-ppk")
	}
}

func TestPPKComment(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pub := bytes.Join([][]byte{encStr([]byte("ssh-rsa")), encMpintBig(big.NewInt(int64(key.E))), encMpintBig(key.N)}, nil)
	iqmp := new(big.Int).ModInverse(key.Primes[1], key.Primes[0])
	priv := bytes.Join([][]byte{encMpintBig(key.D), encMpintBig(key.Primes[0]), encMpintBig(key.Primes[1]), encMpintBig(iqmp)}, nil)
	ppk := buildPPK(t, 3, "ssh-rsa", "my-azure-key", pub, priv, "")
	if got := PPKComment(ppk); got != "my-azure-key" {
		t.Fatalf("PPKComment = %q, want %q", got, "my-azure-key")
	}
	if got := PPKComment("not a ppk"); got != "" {
		t.Fatalf("PPKComment(non-ppk) = %q, want empty", got)
	}
}
