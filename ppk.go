// PPK (PuTTY) -> OpenSSH private key conversion, ported from the Node
// version in ssh-terminal-app/lib/ppk.js. Pure Go, testable on any platform.
// Supports PPK v2 and v3, RSA / ECDSA / Ed25519, plain and passphrase-
// encrypted. v3 uses Argon2 (id/i via golang.org/x/crypto/argon2;
// Argon2d is not exposed by that package and is rejected clearly).
// DSA and Ed448 are rejected clearly.
package main

import (
	"bytes"
	"crypto/aes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

func sshStr(b []byte) []byte {
	out := make([]byte, 4, 4+len(b))
	binary.BigEndian.PutUint32(out, uint32(len(b)))
	return append(out, b...)
}

func mpintEncodeUnsigned(u []byte) []byte {
	for len(u) > 1 && u[0] == 0 {
		u = u[1:]
	}
	if len(u) == 0 {
		u = []byte{0}
	}
	if u[0]&0x80 != 0 {
		u = append([]byte{0}, u...)
	}
	return sshStr(u)
}

func mpintFromBytes(b []byte) []byte {
	for len(b) > 1 && b[0] == 0 {
		b = b[1:]
	}
	return b
}

type ppkReader struct {
	buf []byte
	off int
}

func (r *ppkReader) readString() ([]byte, error) {
	if r.off+4 > len(r.buf) {
		return nil, errors.New("blob .ppk terpotong")
	}
	n := int(binary.BigEndian.Uint32(r.buf[r.off:]))
	r.off += 4
	if n < 0 || r.off+n > len(r.buf) {
		return nil, errors.New("blob .ppk terpotong")
	}
	out := r.buf[r.off : r.off+n]
	r.off += n
	return out, nil
}

func (r *ppkReader) readMpint() ([]byte, error) {
	b, err := r.readString()
	if err != nil {
		return nil, err
	}
	return mpintFromBytes(b), nil
}

type ppkHeaders struct {
	version   int
	alg       string
	encryption string
	comment   string
	public    []byte
	private   []byte
	macHex    string
	kdf       string
	argMemory int
	argPasses int
	argPar    int
	argSalt   []byte
}

func parsePPKHeaders(text string) (*ppkHeaders, error) {
	text = strings.TrimPrefix(text, "\ufeff")
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = strings.TrimSuffix(lines[i], "\r")
	}
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "PuTTY-User-Key-File-") {
		return nil, errors.New("not a PuTTY .ppk file")
	}
	// PuTTY-User-Key-File-3: ssh-rsa
	first := lines[0]
	colon := strings.Index(first, ":")
	if colon < 0 {
		return nil, errors.New(".ppk header cannot be read")
	}
	verStr := strings.TrimPrefix(first[:colon], "PuTTY-User-Key-File-")
	ver, err := strconv.Atoi(verStr)
	if err != nil {
		return nil, errors.New(".ppk version cannot be read")
	}
	h := &ppkHeaders{version: ver, alg: strings.TrimSpace(first[colon+1:])}
	i := 1
	for i < len(lines) {
		line := lines[i]
		c := strings.Index(line, ":")
		if c < 0 {
			i++
			continue
		}
		key := strings.TrimSpace(line[:c])
		val := strings.TrimSpace(line[c+1:])
		i++
		switch key {
		case "Encryption":
			h.encryption = val
		case "Comment":
			h.comment = val
		case "Key-Derivation":
			h.kdf = val
		case "Argon2-Memory":
			h.argMemory, _ = strconv.Atoi(val)
		case "Argon2-Passes":
			h.argPasses, _ = strconv.Atoi(val)
		case "Argon2-Parallelism":
			h.argPar, _ = strconv.Atoi(val)
		case "Argon2-Salt":
			h.argSalt, err = hex.DecodeString(val)
			if err != nil {
				return nil, errors.New("invalid .ppk Argon2-Salt")
			}
		case "Public-Lines", "Private-Lines":
			n, err := strconv.Atoi(val)
			if err != nil || n < 0 || i+n > len(lines) {
				return nil, errors.New("file .ppk terpotong")
			}
			blob := ""
			for j := 0; j < n; j++ {
				blob += strings.TrimSpace(lines[i+j])
			}
			// base64 decode: use stdlib via hex? Use encoding/base64
			dec, err := decodeB64(blob)
			if err != nil {
				return nil, errors.New("invalid .ppk base64")
			}
			if key == "Public-Lines" {
				h.public = dec
			} else {
				h.private = dec
			}
			i += n
		case "Private-MAC":
			h.macHex = strings.ToLower(val)
		}
	}
	return h, nil
}

func deriveV2(passphrase string) (cipherKey, iv, macKey []byte) {
	pass := []byte(passphrase)
	h1 := sha1.Sum(append([]byte{0, 0, 0, 0}, pass...))
	h2 := sha1.Sum(append([]byte{0, 0, 0, 1}, pass...))
	cipherKey = append(h1[:], h2[:]...)[:32]
	iv = make([]byte, 16)
	m := sha1.Sum(append([]byte("putty-private-key-file-mac-key"), pass...))
	macKey = m[:]
	return cipherKey, iv, macKey
}

func deriveV3(passphrase string, h *ppkHeaders) (cipherKey, iv, macKey []byte, err error) {
	if h.argMemory <= 0 || h.argPasses <= 0 || h.argPar <= 0 || len(h.argSalt) == 0 {
		return nil, nil, nil, errors.New("incomplete Argon2 parameters in .ppk")
	}
	var derived []byte
	switch h.kdf {
	case "Argon2id":
		derived = argon2.IDKey([]byte(passphrase), h.argSalt, uint32(h.argPasses), uint32(h.argMemory), uint8(h.argPar), 80)
	case "Argon2i":
		derived = argon2.Key([]byte(passphrase), h.argSalt, uint32(h.argPasses), uint32(h.argMemory), uint8(h.argPar), 80)
	default:
		return nil, nil, nil, fmt.Errorf(".ppk Key-Derivation not supported: %s (supported: Argon2id, Argon2i)", h.kdf)
	}
	return derived[:32], derived[32:48], derived[48:80], nil
}

// PPKComment returns the "Comment:" field from a .ppk file header, or ""
// if the file is not a .ppk or has no comment. The comment is plaintext
// in the header even for passphrase-encrypted keys, so this works
// without the passphrase and without decrypting the key.
func PPKComment(ppkText string) string {
	if !strings.Contains(ppkText, "PuTTY-User-Key-File-") {
		return ""
	}
	h, err := parsePPKHeaders(ppkText)
	if err != nil {
		return ""
	}
	return h.comment
}

// PPKToOpenSSH converts PPK text to an unencrypted OpenSSH PEM private key.
// passphrase is the PPK passphrase (empty for unencrypted keys).
func PPKToOpenSSH(ppkText, passphrase string) (pem []byte, alg string, err error) {
	if !strings.Contains(ppkText, "PuTTY-User-Key-File-") {
		return nil, "", errors.New("not a .ppk file")
	}
	h, err := parsePPKHeaders(ppkText)
	if err != nil {
		return nil, "", err
	}
	if h.version != 2 && h.version != 3 {
		return nil, "", fmt.Errorf(".ppk version %d not supported (only v2 & v3)", h.version)
	}
	if len(h.public) == 0 || len(h.private) == 0 {
		return nil, "", errors.New("this .ppk file contains only a public key (no private key)")
	}
	if h.macHex == "" {
		return nil, "", errors.New(".ppk Private-MAC not found")
	}

	var cipherKey, iv, macKey []byte
	privateBuf := h.private
	if h.encryption == "none" {
		if h.version == 2 {
			m := sha1.Sum([]byte("putty-private-key-file-mac-key"))
			macKey = m[:]
		} else {
			macKey = []byte{}
		}
	} else if h.encryption == "aes256-cbc" {
		if passphrase == "" {
			return nil, "", errors.New("this .ppk key is encrypted — enter the key passphrase")
		}
		if h.version == 2 {
			cipherKey, iv, macKey = deriveV2(passphrase)
		} else {
			cipherKey, iv, macKey, err = deriveV3(passphrase, h)
			if err != nil {
				return nil, "", err
			}
		}
		if len(h.private)%aes.BlockSize != 0 {
			return nil, "", errors.New(".ppk private blob is not a multiple of the AES block size")
		}
		block, err := aes.NewCipher(cipherKey)
		if err != nil {
			return nil, "", err
		}
		dec := make([]byte, len(h.private))
		// CBC decrypt, no padding (PuTTY uses random padding)
		prev := iv
		for off := 0; off < len(h.private); off += aes.BlockSize {
			blk := h.private[off : off+aes.BlockSize]
			tmp := make([]byte, aes.BlockSize)
			block.Decrypt(tmp, blk)
			for j := range tmp {
				tmp[j] ^= prev[j]
			}
			copy(dec[off:], tmp)
			prev = blk
		}
		privateBuf = dec
	} else {
		return nil, "", fmt.Errorf(".ppk encryption not supported: %s", h.encryption)
	}

	// MAC verification
	macPre := bytes.Join([][]byte{
		sshStr([]byte(h.alg)),
		sshStr([]byte(h.encryption)),
		sshStr([]byte(h.comment)),
		sshStr(h.public),
		sshStr(privateBuf),
	}, nil)
	var computed []byte
	if h.version == 2 {
		m := hmac.New(sha1.New, macKey)
		m.Write(macPre)
		computed = m.Sum(nil)
	} else {
		m := hmac.New(sha256.New, macKey)
		m.Write(macPre)
		computed = m.Sum(nil)
	}
	expected, err := hex.DecodeString(h.macHex)
	if err != nil || !hmac.Equal(expected, computed) {
		return nil, "", errors.New(".ppk MAC mismatch — wrong passphrase, or the .ppk file is corrupt / has been modified")
	}

	// Public blob
	pubR := &ppkReader{buf: h.public}
	pubAlgBytes, err := pubR.readString()
	if err != nil {
		return nil, "", err
	}
	if string(pubAlgBytes) != h.alg {
		return nil, "", errors.New(".ppk public key algorithm does not match the header")
	}
	privR := &ppkReader{buf: privateBuf}
	var privFields []byte
	switch h.alg {
	case "ssh-rsa":
		e, err := pubR.readMpint()
		if err != nil {
			return nil, "", err
		}
		n, err := pubR.readMpint()
		if err != nil {
			return nil, "", err
		}
		d, err := privR.readMpint()
		if err != nil {
			return nil, "", err
		}
		p, err := privR.readMpint()
		if err != nil {
			return nil, "", err
		}
		q, err := privR.readMpint()
		if err != nil {
			return nil, "", err
		}
		iqmp, err := privR.readMpint()
		if err != nil {
			return nil, "", err
		}
		privFields = bytes.Join([][]byte{
			sshStr([]byte("ssh-rsa")),
			mpintEncodeUnsigned(n), mpintEncodeUnsigned(e), mpintEncodeUnsigned(d),
			mpintEncodeUnsigned(iqmp), mpintEncodeUnsigned(p), mpintEncodeUnsigned(q),
		}, nil)
	case "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521":
		curve, err := pubR.readString()
		if err != nil {
			return nil, "", err
		}
		point, err := pubR.readString()
		if err != nil {
			return nil, "", err
		}
		d, err := privR.readMpint()
		if err != nil {
			return nil, "", err
		}
		privFields = bytes.Join([][]byte{
			sshStr([]byte(h.alg)),
			sshStr(curve),
			sshStr(point),
			mpintEncodeUnsigned(d),
		}, nil)
	case "ssh-ed25519":
		pubKey, err := pubR.readString()
		if err != nil {
			return nil, "", err
		}
		seedRaw, err := privR.readString()
		if err != nil {
			return nil, "", err
		}
		var seed []byte
		if len(seedRaw) == 64 {
			seed = seedRaw[:32]
		} else {
			seed = mpintFromBytes(seedRaw)
			if len(seed) > 32 {
				return nil, "", errors.New("invalid Ed25519 private key in .ppk")
			}
			if len(seed) < 32 {
				pad := make([]byte, 32-len(seed))
				seed = append(pad, seed...)
			}
		}
		if len(pubKey) != 32 {
			return nil, "", errors.New("Ed25519 public key in .ppk is not 32 bytes")
		}
		priv64 := append(append([]byte{}, seed...), pubKey...)
		privFields = bytes.Join([][]byte{
			sshStr([]byte("ssh-ed25519")),
			sshStr(pubKey),
			sshStr(priv64),
		}, nil)
	default:
		return nil, "", fmt.Errorf(".ppk key type not supported: %s (supported: ssh-rsa, ecdsa-sha2-nistp*, ssh-ed25519)", h.alg)
	}

	// Build openssh-key-v1 (unencrypted)
	check := make([]byte, 4)
	if _, err := rand.Read(check); err != nil {
		return nil, "", err
	}
	privSection := bytes.Join([][]byte{check, check, privFields, sshStr([]byte(h.comment))}, nil)
	if padLen := (8 - (len(privSection) % 8)) % 8; padLen != 0 {
		pad := make([]byte, padLen)
		for i := range pad {
			pad[i] = byte(i + 1)
		}
		privSection = append(privSection, pad...)
	}
	// uint32 1
	nkeys := make([]byte, 4)
	binary.BigEndian.PutUint32(nkeys, 1)
	fileBuf := bytes.Join([][]byte{
		[]byte("openssh-key-v1\x00"),
		sshStr([]byte("none")),
		sshStr([]byte("none")),
		sshStr([]byte{}),
		nkeys,
		sshStr(h.public),
		sshStr(privSection),
	}, nil)
	// PEM wrap at 70 chars, using big.Int? No: base64 encode
	b64 := encodeB64(fileBuf)
	var sb strings.Builder
	sb.WriteString("-----BEGIN OPENSSH PRIVATE KEY-----\n")
	for i := 0; i < len(b64); i += 70 {
		end := i + 70
		if end > len(b64) {
			end = len(b64)
		}
		sb.WriteString(b64[i:end])
		sb.WriteString("\n")
	}
	sb.WriteString("-----END OPENSSH PRIVATE KEY-----\n")
	return []byte(sb.String()), h.alg, nil
}
